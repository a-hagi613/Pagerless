package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// JSON keys the broker adds to a bounded preview.
const (
	fieldTruncated = "truncated"
	fieldFullBytes = "fullBytes"
	fieldPreview   = "preview"
)

// The four budgets and the byte ceiling (spec §3.6.1 and §3.6.3). The agent
// enforces all four locally; the server enforces the tool-call count and an
// independent byte ceiling, which is the only wall that survives a compromised
// agent binary.
const (
	// DefaultToolCalls is the number of tool calls one investigation may make.
	DefaultToolCalls = 15
	// DefaultModelCalls is the number of model calls one investigation may make.
	DefaultModelCalls = 12
	// DefaultInputTokens is the input-token budget of one investigation.
	DefaultInputTokens int64 = 300_000
	// DefaultSeconds is the wall-clock budget of one investigation.
	DefaultSeconds = 300.0
	// MaxResultBytes bounds the JSON of one tool result.
	MaxResultBytes = 8 << 10
	// MaxPageBytes bounds what the broker returns to one Page over its lifetime.
	MaxPageBytes = 64 << 10
	// MaxManifestBytes bounds the Context Bundle in the manifest.
	MaxManifestBytes = 64 << 10
)

// ContextSource supplies the Page's stored Context Bundle. The production
// implementation wraps internal/contextbundle; a Page with no bundle returns an
// empty object.
type ContextSource interface {
	// Context returns the sanitized bundle as JSON.
	Context(ctx context.Context, page *v1alpha1.Page) (json.RawMessage, error)
}

// Broker serves the read-only tool API for one Page at a time.
type Broker struct {
	// Reader reads Kubernetes objects.
	Reader client.Reader
	// PodLogs reads the pod log subresource.
	PodLogs PodLogReader
	// GitHub reads the Target Repository.
	GitHub GitHubReader
	// Connectors is the configured Connector catalogue. A nil value disables
	// the connector tools, which then answer with a normal tool error.
	Connectors ConnectorSource
	// Context supplies the Context Bundle.
	Context ContextSource
	// Store is the evidence record.
	Store Store
	// Sink records calls and SuspiciousOutput events on the Page.
	Sink Sink
	// Clock stamps calls. It defaults to the real clock.
	Clock clock.PassiveClock
	// Limits are the server-advertised budgets. Zero values select the defaults.
	Limits Limits
	// Evidence names the store's durability in the manifest.
	Evidence string
	// refusalSeq makes each refused call's identity unique, so repeated
	// refusals are counted rather than overwritten.
	refusalSeq atomic.Int64
}

// scope returns the enforced scope of one Page.
func (b *Broker) scope(page *v1alpha1.Page) Scope {
	scope := scopeFor(page)
	if b.Connectors != nil {
		scope.Connectors = b.Connectors.Names()
	}
	return scope
}

// limits returns the budgets, with the documented defaults filled in.
func (b *Broker) limits() Limits {
	limits := b.Limits
	if limits.ToolCalls <= 0 {
		limits.ToolCalls = DefaultToolCalls
	}
	if limits.ModelCalls <= 0 {
		limits.ModelCalls = DefaultModelCalls
	}
	if limits.InputTokens <= 0 {
		limits.InputTokens = DefaultInputTokens
	}
	if limits.Seconds <= 0 {
		limits.Seconds = DefaultSeconds
	}
	return limits
}

// now returns the broker's clock reading.
func (b *Broker) now() time.Time {
	if b.Clock == nil {
		return time.Now()
	}
	return b.Clock.Now()
}

// Manifest returns the investigation manifest: Context Bundle, catalogue,
// scope, and limits in one response.
func (b *Broker) Manifest(ctx context.Context, page *v1alpha1.Page) (Manifest, error) {
	bundle := json.RawMessage(`{}`)
	if b.Context != nil {
		collected, err := b.Context.Context(ctx, page)
		if err != nil {
			return Manifest{}, fmt.Errorf("read the Context Bundle: %w", err)
		}
		if len(collected) > 0 {
			if json.Valid(collected) {
				bundle = collected
			} else {
				return Manifest{}, fmt.Errorf("the stored Context Bundle is not JSON")
			}
		}
	}
	bundle = boundJSON(bundle, MaxManifestBytes)
	manifest := Manifest{
		Page:    Page{Name: page.Name, UID: string(page.UID), Namespace: page.Namespace},
		Target:  targetFor(page),
		Scope:   b.scope(page),
		Context: bundle,
		Tools:   catalogue(),
		Limits:  b.limits(),
		Hint:    page.Spec.Hint,
		Capabilities: &Capabilities{
			Envelope: EnvelopeTag,
			Evidence: b.Evidence,
		},
	}
	trigger := &Trigger{
		Kind:   string(page.Spec.Trigger.Source),
		Reason: page.Spec.Trigger.Reason,
		Pod:    page.Spec.Trigger.Pod,
	}
	manifest.Trigger = trigger
	return manifest, nil
}

// targetFor renders the Page's Target.
func targetFor(page *v1alpha1.Page) Target {
	target := Target{
		Kind:      string(page.Spec.Target.Kind),
		Name:      page.Spec.Target.Name,
		Namespace: page.Spec.Target.Namespace,
		UID:       page.Spec.Target.UID,
	}
	return target
}

// Call serves one tool call. It returns either a result or a refusal with the
// HTTP status the handler should answer with.
func (b *Broker) Call(
	ctx context.Context, page *v1alpha1.Page, tool string, arguments json.RawMessage, idempotency string,
) (Result, *Refusal, int) {
	pageUID := string(page.UID)
	scope := b.scope(page)
	if !inCatalogue(tool) {
		b.raiseUnknown(ctx, page, tool)
		return Result{}, refusal(CodeUnknownTool, "the tool "+tool+" is not in the catalogue"), 404
	}
	if b.containsEnvelopeBoundary(arguments) {
		b.raise(ctx, page, "a tool call tried to forge a tool_output envelope boundary")
	}
	served, err := b.Store.Served(ctx, pageUID)
	if err != nil {
		return Result{}, refusal(CodeToolError, "the evidence record could not be read"), 500
	}
	limits := b.limits()
	if served >= limits.ToolCalls {
		b.recordRefusal(ctx, page, tool, arguments, CodeBudgetExhausted)
		return Result{}, refusal(CodeBudgetExhausted,
			fmt.Sprintf("all %d tool calls are spent", limits.ToolCalls)), 429
	}
	spent, err := b.Store.Bytes(ctx, pageUID)
	if err != nil {
		return Result{}, refusal(CodeToolError, "the evidence record could not be read"), 500
	}
	if spent >= MaxPageBytes {
		b.recordRefusal(ctx, page, tool, arguments, CodeBudgetExhausted)
		return Result{}, refusal(CodeBudgetExhausted,
			fmt.Sprintf("the %d byte result ceiling is reached", MaxPageBytes)), 429
	}
	callID := b.callID(pageUID, idempotency, served+1)
	if stored, ok, err := b.Store.Get(ctx, pageUID, callID); err == nil && ok {
		// An idempotent retry returns the recorded result without a second read.
		var data any
		if err := json.Unmarshal(stored.Data, &data); err != nil {
			return Result{}, refusal(CodeToolError, "the recorded result could not be decoded"), 500
		}
		return Result{OK: true, Data: data, Meta: Meta{
			CallID: callID, Tool: stored.Tool, Bytes: int(stored.Bytes), Redactions: stored.Redactions,
		}}, nil, 200
	}

	data, err := b.dispatch(ctx, page, scope, tool, arguments)
	if err != nil {
		code, status := classify(err)
		if errors.Is(err, ErrOutOfScope) {
			b.raiseOutOfScope(ctx, page, tool)
		}
		b.recordRefusal(ctx, page, tool, arguments, code)
		return Result{}, refusal(code, err.Error()), status
	}
	redacted, redactions := redactResult(data)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return Result{}, refusal(CodeToolError, "the result could not be encoded"), 500
	}
	truncated := false
	if len(encoded) > MaxResultBytes {
		encoded, truncated = previewJSON(encoded, MaxResultBytes)
		var preview map[string]any
		if err := json.Unmarshal(encoded, &preview); err == nil {
			redacted = preview
		}
	}
	if spent+int64(len(encoded)) > MaxPageBytes {
		b.recordRefusal(ctx, page, tool, arguments, CodeBudgetExhausted)
		return Result{}, refusal(CodeBudgetExhausted,
			fmt.Sprintf("the next result would exceed the %d byte result ceiling", MaxPageBytes)), 429
	}
	record := Record{
		PageUID: pageUID, CallID: callID, Tool: tool, Arguments: boundString(string(arguments), 300),
		Data: encoded, Outcome: "ok", Bytes: int64(len(encoded)), Redactions: redactions, At: b.now(),
	}
	if err := b.Store.Record(ctx, record); err != nil {
		return Result{}, refusal(CodeToolError, "the result could not be recorded"), 500
	}
	if b.Sink != nil {
		_ = b.Sink.RecordToolCall(ctx, page, record)
	}
	return Result{OK: true, Data: redacted, Meta: Meta{
		CallID: callID, Tool: tool, Bytes: len(encoded), Redactions: redactions, Truncated: truncated,
	}}, nil, 200
}

// dispatch runs one tool after the scope check.
func (b *Broker) dispatch(
	ctx context.Context, page *v1alpha1.Page, scope Scope, tool string, arguments json.RawMessage,
) (any, error) {
	kube := &kubeTools{reader: b.Reader, podLogs: b.PodLogs, now: b.now}
	switch tool {
	case ToolObjectGet:
		return kube.get(ctx, page, scope, arguments)
	case ToolObjectList:
		return kube.list(ctx, page, scope, arguments)
	case ToolPodsLogs:
		return kube.logs(ctx, page, scope, arguments)
	case ToolEventsList:
		return kube.events(ctx, page, scope, arguments)
	case ToolOwnerChain:
		return kube.ownerChain(ctx, page, scope, arguments)
	case ToolConnectorLogs, ToolConnectorMetrics, ToolConnectorHealth:
		return b.connector(ctx, page, scope, tool, arguments)
	case ToolRepoTree, ToolRepoFile, ToolRepoCommits:
		return b.repository(ctx, scope, tool, arguments)
	default:
		return nil, fmt.Errorf("%w: the tool %s is not in the catalogue", ErrOutOfScope, tool)
	}
}

// callID derives the identifier a Finding cites. It is deterministic in the
// Page and the agent's Idempotency-Key, so a transport retry cannot double a
// read and cannot move a citation.
func (b *Broker) callID(pageUID, idempotency string, ordinal int) string {
	if idempotency == "" {
		return fmt.Sprintf("call-%d", ordinal)
	}
	sum := sha256.Sum256([]byte(pageUID + "\x00" + idempotency))
	return "call-" + hex.EncodeToString(sum[:8])
}

// recordRefusal stores a refused call so the Page can show what was attempted.
func (b *Broker) recordRefusal(
	ctx context.Context, page *v1alpha1.Page, tool string, arguments json.RawMessage, outcome string,
) {
	record := Record{
		PageUID: string(page.UID),
		CallID:  fmt.Sprintf("refused-%d-%s", b.refusalSeq.Add(1), tool), Tool: tool,
		Arguments: boundString(string(arguments), 300), Outcome: outcome, At: b.now(),
	}
	if b.Store != nil {
		_ = b.Store.Record(ctx, record)
	}
	if b.Sink != nil {
		_ = b.Sink.RecordToolCall(ctx, page, record)
	}
}

// raiseUnknown records an out-of-catalogue attempt and raises one event.
func (b *Broker) raiseUnknown(ctx context.Context, page *v1alpha1.Page, tool string) {
	if b.Store == nil {
		return
	}
	count, err := b.Store.Refusals(ctx, string(page.UID), tool)
	if err != nil {
		return
	}
	if count == 1 {
		b.raise(ctx, page, fmt.Sprintf("the agent asked for the tool %q, which is not in the catalogue", tool))
	}
}

// raiseOutOfScope records an out-of-scope attempt and raises one event on the
// second attempt.
func (b *Broker) raiseOutOfScope(ctx context.Context, page *v1alpha1.Page, tool string) {
	if b.Store == nil {
		return
	}
	count, err := b.Store.Refusals(ctx, string(page.UID), "out-of-scope/"+tool)
	if err != nil {
		return
	}
	if count == 2 {
		b.raise(ctx, page, "the agent repeated a read outside the Page's scope")
	}
}

// raise records one SuspiciousOutput event on the Page.
func (b *Broker) raise(ctx context.Context, page *v1alpha1.Page, event string) {
	if b.Sink == nil {
		return
	}
	_ = b.Sink.RecordSuspiciousOutput(ctx, page, event)
}

// containsEnvelopeBoundary reports whether arguments try to forge the
// untrusted-data envelope.
func (b *Broker) containsEnvelopeBoundary(arguments json.RawMessage) bool {
	lower := strings.ToLower(string(arguments))
	for _, boundary := range []string{
		"</" + EnvelopeTag, "<" + EnvelopeTag,
		`\u003c/` + EnvelopeTag, `\u003c` + EnvelopeTag,
	} {
		if strings.Contains(lower, boundary) {
			return true
		}
	}
	return false
}

// classify maps a tool error to its code and HTTP status.
func classify(err error) (string, int) {
	switch {
	case errors.Is(err, ErrOutOfScope):
		return CodeOutOfScope, 403
	case errors.Is(err, ErrInvalidArguments):
		return CodeInvalidArguments, 400
	default:
		return CodeToolError, 502
	}
}

// refusal builds one refusal body.
func refusal(code, message string) *Refusal {
	return &Refusal{OK: false, Error: RefusalError{Code: code, Message: message}}
}

// boundJSON cuts a JSON document to a byte ceiling, replacing an oversized
// document with a valid prefix object.
func boundJSON(document json.RawMessage, limit int) json.RawMessage {
	if len(document) <= limit {
		return document
	}
	preview := string(document[:limit-100])
	encoded, err := json.Marshal(map[string]any{
		fieldTruncated: true, fieldFullBytes: len(document), fieldPreview: preview,
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return encoded
}

// previewJSON replaces an oversized result with a valid preview document.
func previewJSON(document []byte, limit int) ([]byte, bool) {
	preview := string(document[:limit-100])
	encoded, err := json.Marshal(map[string]any{
		fieldTruncated: true, fieldFullBytes: len(document), fieldPreview: preview,
	})
	if err != nil {
		return document, false
	}
	return encoded, true
}

// boundString cuts a string to a rune-safe prefix.
func boundString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

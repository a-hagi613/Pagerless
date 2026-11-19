package reportapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/agentauth"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/tools"
)

// maxBodyBytes bounds a request body. Agent output is untrusted.
const maxBodyBytes = 1 << 20

// ContentStore holds a Proposal's complete file content under its hash.
type ContentStore interface {
	// SaveProposalContents records the content of each edited file.
	SaveProposalContents(ctx context.Context, hash string, files map[string][]byte, at time.Time) error
}

// Handler serves the agent API.
type Handler struct {
	// Auth verifies that the caller is the Page's Job.
	Auth *Authenticator
	// Status is the only writer of Page status.
	Status *page.StatusStore
	// Proposals validates Proposals.
	Proposals *proposal.Pipeline
	// Evidence is the record of the tool calls a Finding may cite.
	Evidence tools.Store
	// Blobs reads the Target Repository, so a FilePatch records the base blob
	// and content digest of every file it changes.
	Blobs tools.GitHubReader
	// Contents stores the complete content under the Proposal hash.
	Contents ContentStore
	// Clock timestamps steps.
	Clock clock.PassiveClock
}

// errNotInvestigating is returned when a request needs an Investigating Page and the Page is in another phase.
var errNotInvestigating = errors.New("the Page is not Investigating")

// errReportRaced is returned when another report for the same Page was recorded while this one was validated.
var errReportRaced = errors.New("another report for the Page was recorded first")

// errNoFindings is returned when a report would leave the Diagnosis without a cited Finding.
var errNoFindings = errors.New("the report must carry at least one finding")

// errNotVerified is returned when a Narrative request arrives before Verification has reached a terminal outcome.
var errNotVerified = errors.New("the Page has not reached a terminal Verification outcome")

// errNarrativeRecorded is returned when the Page already stores a different Narrative.
var errNarrativeRecorded = errors.New("the Page already records a different Narrative")

// ServeHTTP routes requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pages/{name}/events", h.handle(h.events))
	mux.HandleFunc("POST /v1/pages/{name}/report", h.handle(h.report))
	mux.HandleFunc("GET /v1/pages/{name}/narrative", h.handleNarrative(h.narrativeRecord))
	mux.HandleFunc("POST /v1/pages/{name}/narrative", h.handleNarrative(h.narrative))
	mux.ServeHTTP(w, r)
}

type endpoint func(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Page)

// authorizer checks that the authenticated caller may act on the Page.
type authorizer func(ctx context.Context, caller *Caller, t *v1alpha1.Page) error

// handle authenticates the caller as the Page's investigation Job, then runs the endpoint.
func (h *Handler) handle(next endpoint) http.HandlerFunc {
	return h.serve(func(ctx context.Context, caller *Caller, t *v1alpha1.Page) error {
		return h.Auth.Authorize(ctx, caller, t)
	}, next)
}

// handleNarrative authenticates the caller as the Page's Narrative Job, then runs the endpoint.
func (h *Handler) handleNarrative(next endpoint) http.HandlerFunc {
	return h.serve(func(ctx context.Context, caller *Caller, t *v1alpha1.Page) error {
		return h.Auth.AuthorizeNarrative(ctx, caller, t)
	}, next)
}

// serve authenticates the caller, finds the Page, authorizes the caller for it, and runs the endpoint.
func (h *Handler) serve(authorize authorizer, next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var t v1alpha1.Page
		key := types.NamespacedName{Namespace: h.Auth.Namespace, Name: r.PathValue("name")}
		if err := h.Status.Reader.Get(ctx, key, &t); err != nil {
			if apierrors.IsNotFound(err) {
				// An authenticated agent learns nothing from a missing Page that it could not learn from a forbidden one.
				writeJSON(w, http.StatusForbidden, agentapi.ErrorResponse{Message: "forbidden"})
				return
			}
			h.writeInternal(ctx, w, err)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		caller, err := h.Auth.Identify(ctx, strings.TrimSpace(token), agentauth.Audience(&t))
		if err != nil {
			h.writeAuthError(ctx, w, err)
			return
		}
		if err := authorize(ctx, caller, &t); err != nil {
			h.writeAuthError(ctx, w, err)
			return
		}
		next(ctx, w, http.MaxBytesReader(w, r.Body, maxBodyBytes), &t)
	}
}

func (h *Handler) writeAuthError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		writeJSON(w, http.StatusUnauthorized, agentapi.ErrorResponse{Message: "unauthenticated"})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, agentapi.ErrorResponse{Message: "forbidden"})
	default:
		h.writeInternal(ctx, w, err)
	}
}

func (h *Handler) writeInternal(ctx context.Context, w http.ResponseWriter, err error) {
	logf.FromContext(ctx).Error(err, "agent API request failed")
	writeJSON(w, http.StatusInternalServerError, agentapi.ErrorResponse{Message: "internal error"})
}

// decode reads a strict JSON body. It answers the request itself and returns
// false when the body is unusable.
func decode(w http.ResponseWriter, body io.Reader, into any) bool {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, agentapi.ErrorResponse{Message: fmt.Sprintf("body exceeds %d bytes", maxBodyBytes)})
			return false
		}
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Message: "malformed JSON body: " + err.Error()})
		return false
	}
	if decoder.More() {
		writeJSON(w, http.StatusBadRequest, agentapi.ErrorResponse{Message: "malformed JSON body: unexpected data after the object"})
		return false
	}
	return true
}

// events records one step in the bounded step summary.
func (h *Handler) events(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Page) {
	var event agentapi.StepEvent
	if !decode(w, body, &event) {
		return
	}
	if errs := event.Check(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs})
		return
	}
	event = event.Bounded()
	now := metav1.NewTime(h.Clock.Now())
	step := v1alpha1.Step{Time: now, Kind: event.Kind, Tool: event.Tool, Summary: event.Summary}
	updated, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return errNotInvestigating
		}
		status := &latest.Status
		status.Steps = append(status.Steps, step)
		if excess := len(status.Steps) - v1alpha1.MaxSteps; excess > 0 {
			status.Steps = slices.Clone(status.Steps[excess:])
		}
		status.StepCount++
		if event.Kind == v1alpha1.StepKindToolCall {
			status.ToolCallCount++
		}
		if event.Finding != nil {
			status.Findings = mergeFindings(status.Findings, []v1alpha1.Finding{*event.Finding})
		}
		if event.Kind == v1alpha1.StepKindSuspiciousOutput {
			status.SuspiciousOutputs = appendBounded(status.SuspiciousOutputs,
				agentapi.MaxSuspiciousOutputs, event.Summary)
		}
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	// The step is history as it arrives, even though status keeps only the most recent MaxSteps of them.
	h.Status.RecordStep(ctx, updated, step, event.Finding)
	w.WriteHeader(http.StatusAccepted)
}

// report validates the report and, if it passes, records the Diagnosis,
// Findings, and Proposal and moves the Page to Proposed.
func (h *Handler) report(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Page) {
	var report agentapi.Report
	if !decode(w, body, &report) {
		return
	}
	if t.Status.Phase != v1alpha1.PhaseInvestigating {
		h.writeUpdateError(ctx, w, errNotInvestigating)
		return
	}
	// The agent's first report is attempt 1; every report that is refused gives it one more try, up to the limit.
	attempt := t.Status.ValidationAttempts + 1
	errs := report.Check()
	action := report.ActionFor()
	if len(errs) == 0 && report.IsV2() {
		errs = append(errs, h.verifyIdentity(t, report)...)
	}
	if len(errs) == 0 && report.IsV2() {
		errs = append(errs, h.verifyCitations(ctx, t, report.Findings)...)
	}
	contents := map[string][]byte(nil)
	if len(errs) == 0 {
		action, contents, errs = h.prepareFilePatch(ctx, t, action)
	}
	if len(errs) == 0 && len(mergeFindings(t.Status.Findings, report.Findings)) == 0 {
		errs = []agentapi.ValidationError{{Field: "findings", Message: errNoFindings.Error()}}
	}
	if len(errs) > 0 {
		h.refuse(ctx, w, t, attempt, action, errs)
		return
	}
	built, errs, err := h.Proposals.Build(ctx, proposal.Request{Page: t, Action: action}, attempt)
	if err != nil {
		h.writeInternal(ctx, w, err)
		return
	}
	if len(errs) > 0 {
		h.refuse(ctx, w, t, attempt, action, errs)
		return
	}
	if len(contents) > 0 {
		if h.Contents == nil {
			h.writeInternal(ctx, w, errors.New("the Proposal content store is not configured"))
			return
		}
		if err := h.Contents.SaveProposalContents(ctx, built.Hash, contents, h.Clock.Now()); err != nil {
			h.writeInternal(ctx, w, err)
			return
		}
	}
	updated, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return errNotInvestigating
		}
		if latest.Status.ValidationAttempts != attempt-1 {
			return errReportRaced
		}
		findings := mergeFindings(latest.Status.Findings, report.Findings)
		if len(findings) == 0 {
			return errNoFindings
		}
		latest.Status.Findings = findings
		latest.Status.Diagnosis = report.Diagnosis
		latest.Status.Proposal = built
		if report.Usage != nil {
			latest.Status.Usage = report.Usage
		}
		if report.Capabilities != nil {
			if len(report.Capabilities.StructuredOutput) > 0 {
				latest.Status.StructuredOutput = report.Capabilities.StructuredOutput
			}
			latest.Status.UsageSource = report.Capabilities.UsageSource
		}
		if latest.Status.UsageSource == "" && report.Usage != nil && report.Usage.Estimated {
			latest.Status.UsageSource = "estimated"
		}
		latest.Status.Phase = v1alpha1.PhaseProposed
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionInvestigated,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonAgentReported,
			Message:            "the agent reported a validated Proposal",
			ObservedGeneration: latest.Generation,
		})
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentapi.ReportResponse{Accepted: true, Phase: updated.Status.Phase})
}

// prepareFilePatch resolves the base blob and content digest of every file a
// FilePatch changes, and returns the content to store under the Proposal hash.
// It refuses a file that is not in the Target Repository rather than recording a
// Proposal that can never be a reviewable change.
func (h *Handler) prepareFilePatch(
	ctx context.Context, t *v1alpha1.Page, action v1alpha1.ProposedAction,
) (v1alpha1.ProposedAction, map[string][]byte, []agentapi.ValidationError) {
	if action.Action != v1alpha1.ActionFilePatch || action.Parameters == nil || action.Parameters.FilePatch == nil {
		return action, nil, nil
	}
	if h.Blobs == nil {
		return action, nil, []agentapi.ValidationError{{
			Field: "proposal.kind", Message: "repository reads are not configured",
		}}
	}
	repository, ok := tools.RepositoryForPage(t)
	if !ok {
		return action, nil, []agentapi.ValidationError{{
			Field: "proposal.kind", Message: "the Page has no Target Repository, so a FilePatch cannot be grounded",
		}}
	}
	files := action.Parameters.FilePatch.DeepCopy()
	contents := make(map[string][]byte, len(files.Files))
	var errs []agentapi.ValidationError
	for i := range files.Files {
		file := &files.Files[i]
		blob, err := h.Blobs.File(ctx, repository, repository.Ref, file.Path)
		if err != nil {
			errs = append(errs, agentapi.ValidationError{
				Field:   fmt.Sprintf("proposal.files[%d].path", i),
				Message: file.Path + " is not a file at the Target Repository's ref",
			})
			continue
		}
		if blob.SHA == "" {
			errs = append(errs, agentapi.ValidationError{
				Field:   fmt.Sprintf("proposal.files[%d].baseBlob", i),
				Message: "the repository did not report the base blob",
			})
			continue
		}
		sum := sha256.Sum256([]byte(file.UpdatedContent))
		file.BaseBlob = blob.SHA
		file.ContentDigest = hex.EncodeToString(sum[:])
		contents[file.Path] = []byte(file.UpdatedContent)
		file.UpdatedContent = ""
	}
	if len(errs) > 0 {
		return action, nil, errs
	}
	parameters := *action.Parameters
	parameters.FilePatch = files
	action.Parameters = &parameters
	return action, contents, nil
}

// verifyIdentity refuses a report that names a different Page than the one the
// token and the URL identify.
func (h *Handler) verifyIdentity(t *v1alpha1.Page, report agentapi.Report) []agentapi.ValidationError {
	if report.Page == nil {
		return nil
	}
	var errs []agentapi.ValidationError
	if report.Page.Name != t.Name {
		errs = append(errs, agentapi.ValidationError{Field: "page.name", Message: "the report names a different Page"})
	}
	if report.Page.UID != string(t.UID) {
		errs = append(errs, agentapi.ValidationError{Field: "page.uid", Message: "the report names a different Page UID"})
	}
	if report.Page.Namespace != t.Namespace {
		errs = append(errs, agentapi.ValidationError{Field: "page.namespace", Message: "the report names a different namespace"})
	}
	return errs
}

// verifyCitations refuses a Finding that cites a tool call the Page never made,
// or a pointer that does not resolve in the recorded result. It is the
// authoritative check that a Diagnosis cannot be supported by invented evidence.
func (h *Handler) verifyCitations(
	ctx context.Context, t *v1alpha1.Page, findings []v1alpha1.Finding,
) []agentapi.ValidationError {
	if h.Evidence == nil {
		return nil
	}
	var errs []agentapi.ValidationError
	for i, finding := range findings {
		field := fmt.Sprintf("findings[%d]", i)
		if finding.ToolCallID == "" {
			errs = append(errs, agentapi.ValidationError{
				Field: field + ".toolCallId", Message: "every Finding must cite a recorded tool call",
			})
			continue
		}
		record, ok, err := h.Evidence.Get(ctx, string(t.UID), finding.ToolCallID)
		if err != nil {
			errs = append(errs, agentapi.ValidationError{Field: field + ".toolCallId", Message: "the evidence record could not be read"})
			continue
		}
		if !ok || record.Outcome != "ok" {
			errs = append(errs, agentapi.ValidationError{
				Field:   field + ".toolCallId",
				Message: fmt.Sprintf("no tool call named %q was made for this Page", finding.ToolCallID),
			})
			continue
		}
		if finding.Pointer == "" {
			errs = append(errs, agentapi.ValidationError{
				Field: field + ".pointer", Message: "every Finding must name a pointer into its tool result",
			})
			continue
		}
		if !pointerResolves(record, finding.Pointer) {
			errs = append(errs, agentapi.ValidationError{
				Field:   field + ".pointer",
				Message: fmt.Sprintf("the pointer %q does not resolve in the recorded result", finding.Pointer),
			})
		}
	}
	return errs
}

// pointerResolves reports whether a JSON Pointer resolves inside the stored
// tool result, which the agent saw as {ok, data, meta}.
func pointerResolves(record tools.Record, pointer string) bool {
	document := map[string]any{}
	var data any
	if err := json.Unmarshal(record.Data, &data); err != nil {
		return false
	}
	document["data"] = data
	document["meta"] = map[string]any{"callId": record.CallID, "tool": record.Tool}
	current := any(document)
	for token := range strings.SplitSeq(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			value, ok := typed[token]
			if !ok {
				return false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(typed) {
				return false
			}
			current = typed[index]
		default:
			return false
		}
	}
	return true
}

// maxRefusalMessageRunes bounds the ProposalRefused condition message. Two
// refusals fit comfortably inside it.
const maxRefusalMessageRunes = 3000

// refuse records a Proposal that failed Validation and answers with its errors.
// The answer names the Page's phase: Investigating means the agent may
// submit a revised Proposal, and Invalid means the second failure ended the Page.
func (h *Handler) refuse(
	ctx context.Context, w http.ResponseWriter, t *v1alpha1.Page, attempt int32,
	action v1alpha1.ProposedAction, errs []agentapi.ValidationError,
) {
	updated, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return errNotInvestigating
		}
		if latest.Status.ValidationAttempts != attempt-1 {
			return errReportRaced
		}
		latest.Status.ValidationAttempts = attempt
		recordRefusal(latest, attempt, action, errs)
		if attempt >= v1alpha1.MaxValidationAttempts {
			latest.Status.Phase = v1alpha1.PhaseInvalid
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionFailed,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.ReasonValidationFailedTwice,
				Message:            "the second Proposal also failed Validation: " + proposal.Summary(errs),
				ObservedGeneration: latest.Generation,
			})
		}
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs, Phase: updated.Status.Phase})
}

// recordRefusal records a refused attempt on the Investigated condition: the
// Action, the parameters the agent proposed, and the Validation errors. The
// condition stays False until a report passes Validation, so a Page keeps the
// newest refusal visible while the agent revises its Proposal.
func recordRefusal(
	latest *v1alpha1.Page, attempt int32, action v1alpha1.ProposedAction, errs []agentapi.ValidationError,
) {
	line := fmt.Sprintf("attempt %d: %s refused: %s", attempt, proposal.Describe(action), proposal.Summary(errs))
	message := line
	if existing := meta.FindStatusCondition(latest.Status.Conditions, v1alpha1.ConditionInvestigated); existing != nil &&
		existing.Status == metav1.ConditionFalse && existing.Message != "" {
		message = existing.Message + "\n" + line
	}
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionInvestigated,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonValidationRefused,
		Message:            boundRefusalMessage(message),
		ObservedGeneration: latest.Generation,
	})
}

// boundRefusalMessage keeps the newest refusals when the message would exceed
// the bound, because the latest attempt is the one an operator acts on.
func boundRefusalMessage(message string) string {
	runes := []rune(message)
	if len(runes) <= maxRefusalMessageRunes {
		return message
	}
	return string(runes[len(runes)-maxRefusalMessageRunes:])
}

func (h *Handler) writeUpdateError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotInvestigating):
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errNotInvestigating.Error()})
	case errors.Is(err, errReportRaced):
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errReportRaced.Error()})
	case errors.Is(err, errNotVerified):
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errNotVerified.Error()})
	case errors.Is(err, errNarrativeRecorded):
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errNarrativeRecorded.Error()})
	case errors.Is(err, errNoFindings):
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{
			Errors: []agentapi.ValidationError{{Field: "findings", Message: errNoFindings.Error()}},
		})
	default:
		h.writeInternal(ctx, w, err)
	}
}

// narrativeRecord answers the Narrative Job with the Page rendered for description.
func (h *Handler) narrativeRecord(_ context.Context, w http.ResponseWriter, _ io.Reader, t *v1alpha1.Page) {
	if !verificationConcluded(t.Status.Phase) {
		writeJSON(w, http.StatusConflict, agentapi.ErrorResponse{Message: errNotVerified.Error()})
		return
	}
	writeJSON(w, http.StatusOK, agentapi.NarrativeRecord{
		Target: t.Spec.Target,
		Text:   narrativeRecordText(t),
	})
}

// narrative stores the Narrative that the Job wrote. It is the only place the
// Narrative is written, and it changes nothing else in the status: a Job that
// fails or times out never reaches it.
func (h *Handler) narrative(ctx context.Context, w http.ResponseWriter, body io.Reader, t *v1alpha1.Page) {
	var report agentapi.NarrativeReport
	if !decode(w, body, &report) {
		return
	}
	if errs := report.Check(); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, agentapi.ReportResponse{Errors: errs})
		return
	}
	narrative := strings.TrimSpace(report.Narrative)
	_, err := h.Status.Update(ctx, client.ObjectKeyFromObject(t), func(latest *v1alpha1.Page) error {
		if !verificationConcluded(latest.Status.Phase) {
			return errNotVerified
		}
		if latest.Status.Narrative != "" && latest.Status.Narrative != narrative {
			return errNarrativeRecorded
		}
		latest.Status.Narrative = narrative
		return nil
	})
	if err != nil {
		h.writeUpdateError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// verificationConcluded reports whether Verification has reached a terminal outcome.
func verificationConcluded(phase v1alpha1.Phase) bool {
	return phase == v1alpha1.PhaseRecovered || phase == v1alpha1.PhaseNotRecovered
}

// appendBounded appends a value, dropping the oldest when the slice is full.
func appendBounded(values []string, limit int, value string) []string {
	values = append(values, value)
	if len(values) > limit {
		values = slices.Clone(values[len(values)-limit:])
	}
	return values
}

// mergeFindings appends the new Findings that are not already recorded, up to
// the limit. Findings that arrived in step events are therefore kept when the
// report repeats them.
func mergeFindings(existing, incoming []v1alpha1.Finding) []v1alpha1.Finding {
	merged := slices.Clone(existing)
	for _, f := range incoming {
		if len(merged) >= v1alpha1.MaxFindings {
			break
		}
		if !slices.Contains(merged, f) {
			merged = append(merged, f)
		}
	}
	return merged
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

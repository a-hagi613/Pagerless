package dashboardapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/history"
)

// Phase, Step, and Finding are the API's vocabulary: the dashboard renders exactly what the Triage records.
type (
	Phase   = v1alpha1.Phase
	Step    = v1alpha1.Step
	Finding = v1alpha1.Finding
)

// streamHeartbeat is how often an open event stream sends a comment so that an idle connection stays open.
const streamHeartbeat = 15 * time.Second

// Config assembles the dashboard API.
type Config struct {
	// Namespace is the namespace whose Triages the dashboard shows. It is required.
	Namespace string
	// Reader reads Triages through the server's cache, so the dashboard inherits the server's RBAC.
	Reader client.Reader
	// Writer creates Triages and TriageApprovals as pagerless-server. It is required for Start Triage and
	// for dashboard decisions.
	Writer client.Client
	// Workloads lists the Workloads that Start Triage may target. It defaults to Reader.
	Workloads client.Reader
	// WorkloadNamespaces are the namespaces whose Deployments and StatefulSets Start Triage offers. Empty
	// offers none.
	WorkloadNamespaces []string
	// Auth authenticates dashboard requests and serves the sign-in routes. It is required.
	Auth Auth
	// History is the append-only record that supplies Triages whose resource is gone, and their steps.
	History *history.Store
	// Broker carries live steps and Phase changes to open streams.
	Broker *Broker
	// Clock supplies the current time for relative ranges and Grafana windows. It defaults to the real clock.
	Clock clock.PassiveClock
	// Grafana builds the embedded panels and their deep links. Empty fields disable the panels.
	Grafana GrafanaConfig
	// Assets holds the built dashboard, with the Vite output in its dist directory.
	Assets fs.FS
}

// Auth authenticates dashboard requests and serves the sign-in routes. Its
// implementation performs the Keycloak flow; the API only sees the verified
// user and the opaque session cookie.
type Auth interface {
	// User returns the signed-in user of the request, or nil.
	User(r *http.Request) *User
	// SignIn starts the sign-in flow.
	SignIn() http.Handler
	// Callback completes the sign-in flow.
	Callback() http.Handler
	// SignOut ends the session.
	SignOut() http.Handler
	// RealmProxy forwards browser requests for the Keycloak realm when the
	// realm issuer is not directly reachable from the browser, or returns nil.
	RealmProxy() http.Handler
}

// User is the signed-in dashboard user, as dashboardauth reports it.
type User = dashboardauth.User

// Handler serves the dashboard API and the single-page application.
type Handler struct {
	namespace  string
	reader     client.Reader
	writer     client.Client
	workloads  client.Reader
	namespaces []string
	auth       Auth
	history    *history.Store
	broker     *Broker
	clock      clock.PassiveClock
	grafana    GrafanaConfig
	mux        *http.ServeMux
}

// New returns a Handler. The caller registers it on the dashboard listener.
func New(cfg Config) *Handler {
	h := &Handler{
		namespace:  cfg.Namespace,
		reader:     cfg.Reader,
		writer:     cfg.Writer,
		workloads:  cfg.Workloads,
		namespaces: cfg.WorkloadNamespaces,
		auth:       cfg.Auth,
		history:    cfg.History,
		broker:     cfg.Broker,
		clock:      cfg.Clock,
		grafana:    cfg.Grafana,
	}
	if h.workloads == nil {
		h.workloads = cfg.Reader
	}
	if h.clock == nil {
		h.clock = clock.RealClock{}
	}
	h.mux = http.NewServeMux()
	h.mux.Handle("GET /login", h.auth.SignIn())
	h.mux.Handle("GET /auth/callback", h.auth.Callback())
	h.mux.Handle("POST /logout", h.auth.SignOut())
	if proxy := h.auth.RealmProxy(); proxy != nil {
		h.mux.Handle("/realms/", proxy)
		h.mux.Handle("/resources/", proxy)
	}
	h.mux.HandleFunc("GET /signed-out", h.signedOut)
	h.mux.HandleFunc("GET /api/session", h.session)
	h.mux.HandleFunc("GET /api/triages", h.list)
	h.mux.HandleFunc("GET /api/triages/{name}", h.detail)
	h.mux.HandleFunc("GET /api/triages/{name}/events", h.events)
	h.mux.HandleFunc("POST /api/triages", h.start)
	h.mux.HandleFunc("POST /api/triages/{name}/decision", h.decide)
	h.mux.HandleFunc("GET /api/workloads", h.workloadList)
	h.mux.Handle("/api/", http.NotFoundHandler())
	h.mux.Handle("/", NewSPA(cfg.Assets))
	return h
}

// ServeHTTP routes dashboard requests. Every route except the sign-in flow and
// the signed-out page requires a session: the page redirects to sign-in and the
// API answers 401.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if h.publicPath(r) {
		h.mux.ServeHTTP(w, r)
		return
	}
	if h.auth.User(r) == nil {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "sign in to use the dashboard")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// publicPath reports whether the request may proceed without a session. The
// built assets are public because the browser fetches them before rendering
// anything, and they contain no data; proxied Keycloak realm routes are public
// so the browser can complete the sign-in flow on the dashboard port.
func (h *Handler) publicPath(r *http.Request) bool {
	switch r.URL.Path {
	case "/login", "/auth/callback", "/signed-out":
		return true
	}
	if h.auth != nil && h.auth.RealmProxy() != nil {
		if strings.HasPrefix(r.URL.Path, "/realms/") || strings.HasPrefix(r.URL.Path, "/resources/") {
			return true
		}
	}
	return strings.HasPrefix(r.URL.Path, "/assets/")
}

// TriageSummary is one row of the Triage list.
type TriageSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Phase     Phase  `json:"phase"`

	Workload v1alpha1.WorkloadReference `json:"workload"`
	Trigger  v1alpha1.Trigger           `json:"trigger"`

	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	TerminalAt *time.Time `json:"terminalAt,omitempty"`

	// Action is the current Proposal's Action, when there is one.
	Action v1alpha1.Action `json:"action,omitempty"`
	// FollowUpTriage names the Follow-up Triage this Triage started.
	FollowUpTriage string `json:"followUpTriage,omitempty"`
	// Source reports whether the row came from the live Triage resource or only from history.
	Source string `json:"source"`
}

// ListResponse is the body of GET /api/triages.
type ListResponse struct {
	Triages []TriageSummary `json:"triages"`
}

// Verification is the Verification part of a Triage's record.
type Verification struct {
	StartedAt *time.Time                   `json:"startedAt,omitempty"`
	Checks    []v1alpha1.VerificationCheck `json:"checks"`
}

// DetailResponse is the body of GET /api/triages/{name} and of the stream's snapshot event.
type DetailResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Phase     Phase  `json:"phase"`
	Source    string `json:"source"`

	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	TerminalAt *time.Time `json:"terminalAt,omitempty"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`

	Workload      v1alpha1.WorkloadReference `json:"workload"`
	Trigger       v1alpha1.Trigger           `json:"trigger"`
	Hint          string                     `json:"hint,omitempty"`
	FollowsTriage string                     `json:"followsTriage,omitempty"`
	TriggerCount  int32                      `json:"triggerCount"`

	Steps         []Step `json:"steps"`
	StepCount     int32  `json:"stepCount"`
	ToolCallCount int32  `json:"toolCallCount"`

	Usage     *v1alpha1.Usage     `json:"usage,omitempty"`
	Findings  []Finding           `json:"findings"`
	Diagnosis string              `json:"diagnosis,omitempty"`
	Proposal  *v1alpha1.Proposal  `json:"proposal,omitempty"`
	Decision  *v1alpha1.Decision  `json:"decision,omitempty"`
	Execution *v1alpha1.Execution `json:"execution,omitempty"`

	Verification *Verification `json:"verification,omitempty"`

	Narrative      string `json:"narrative,omitempty"`
	FollowUpTriage string `json:"followUpTriage,omitempty"`

	// Grafana is absent when the server has no Grafana base URL configured.
	Grafana *GrafanaLinks `json:"grafana,omitempty"`
}

// errNotFound is returned when neither the live resource nor history knows the Triage.
var errNotFound = errors.New("the Triage does not exist")

// list answers with the Triages that match the filters, most recent first. It starts from the latest history
// snapshot of every Triage and overlays the live resource, so the list survives retention.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	phase := Phase(query.Get("phase"))
	trigger := v1alpha1.TriggerSource(query.Get("trigger"))
	workload := strings.TrimSpace(query.Get("workload"))
	if phase != "" && !knownPhase(phase) {
		writeError(w, http.StatusBadRequest, "unknown phase "+string(phase))
		return
	}
	if trigger != "" && !knownTrigger(trigger) {
		writeError(w, http.StatusBadRequest, "unknown trigger source "+string(trigger))
		return
	}
	since, err := parseSince(query.Get("since"), h.clock.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	snapshots, err := h.history.Latest(ctx)
	if err != nil {
		h.internal(w, ctx, err)
		return
	}
	var live v1alpha1.TriageList
	if err := h.reader.List(ctx, &live, client.InNamespace(h.namespace)); err != nil {
		h.internal(w, ctx, err)
		return
	}

	type entry struct {
		snapshot *history.Snapshot
		live     *v1alpha1.Triage
	}
	entries := make(map[string]*entry, len(snapshots)+len(live.Items))
	for i := range snapshots {
		snapshot := &snapshots[i]
		if snapshot.Namespace != h.namespace {
			continue
		}
		entries[snapshot.Triage] = &entry{snapshot: snapshot}
	}
	for i := range live.Items {
		tr := &live.Items[i]
		e, ok := entries[tr.Name]
		if !ok {
			e = &entry{}
			entries[tr.Name] = e
		}
		e.live = tr
	}

	summaries := make([]TriageSummary, 0, len(entries))
	for _, e := range entries {
		summary := h.summarize(e.snapshot, e.live)
		if phase != "" && summary.Phase != phase {
			continue
		}
		if trigger != "" && summary.Trigger.Source != trigger {
			continue
		}
		if workload != "" && summary.Workload.Name != workload {
			continue
		}
		if !since.IsZero() && summary.CreatedAt.Before(since) {
			continue
		}
		summaries = append(summaries, summary)
	}
	slices.SortFunc(summaries, func(a, b TriageSummary) int {
		first, second := sortTime(a), sortTime(b)
		if !first.Equal(second) {
			if first.After(second) {
				return -1
			}
			return 1
		}
		return strings.Compare(b.Name, a.Name)
	})
	writeJSON(w, http.StatusOK, ListResponse{Triages: summaries})
}

// detail answers with one Triage's complete record, from the live resource when it exists and from history when
// retention has deleted it.
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	record, err := h.record(r.Context(), r.PathValue("name"))
	if err != nil {
		h.writeRecordError(w, r.Context(), err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// events streams the Triage's steps and Phase changes as Server-Sent Events. The first event is a snapshot of
// the whole record, so a client that connects late still sees the steps it missed.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this connection")
		return
	}
	name := r.PathValue("name")
	// Subscribe before reading the snapshot, so a step that arrives during the read is queued rather than lost.
	// The client deduplicates a step that the snapshot already carries.
	events, unsubscribe := h.broker.Subscribe(name)
	defer unsubscribe()
	record, err := h.record(r.Context(), name)
	if err != nil {
		h.writeRecordError(w, r.Context(), err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Ask reverse proxies not to buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeEvent(w, flusher, EventSnapshot, record); err != nil {
		return
	}
	if record.Phase.IsTerminal() {
		// A Triage that is already terminal cannot change again, so the stream ends after its snapshot.
		_ = writeEvent(w, flusher, EventEnd, PhaseEvent{Phase: record.Phase, Terminal: true})
		return
	}
	heartbeat := time.NewTicker(streamHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-events:
			if !open {
				return
			}
			if err := writeEvent(w, flusher, event.Kind, event.Data); err != nil {
				return
			}
			if event.Kind == EventEnd {
				return
			}
		}
	}
}

// record assembles one Triage from the live resource, or from history when the resource is gone.
func (h *Handler) record(ctx context.Context, name string) (*DetailResponse, error) {
	var tr v1alpha1.Triage
	err := h.reader.Get(ctx, types.NamespacedName{Namespace: h.namespace, Name: name}, &tr)
	switch {
	case err == nil:
		return h.detailFromLive(&tr), nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("read Triage %s: %w", name, err)
	}

	snapshots, err := h.history.Snapshots(ctx, name)
	if err != nil {
		return nil, err
	}
	var latest *history.Snapshot
	for i := range snapshots {
		if snapshots[i].Namespace == h.namespace {
			latest = &snapshots[i]
		}
	}
	if latest == nil {
		return nil, errNotFound
	}
	steps, err := h.history.Steps(ctx, name)
	if err != nil {
		return nil, err
	}
	recorded := make([]Step, 0, len(steps))
	for _, step := range steps {
		if step.Namespace == h.namespace {
			recorded = append(recorded, Step{
				Time: metav1.NewTime(step.RecordedAt), Kind: step.Kind, Tool: step.Tool, Summary: step.Summary,
			})
		}
	}
	return &DetailResponse{
		Name:           latest.Triage,
		Namespace:      latest.Namespace,
		Phase:          latest.Phase,
		Source:         "history",
		CreatedAt:      historyCreatedAt(latest),
		StartedAt:      timePtr(latest.Status.StartedAt),
		TerminalAt:     timePtr(latest.Status.TerminalAt),
		ObservedAt:     &latest.ObservedAt,
		Workload:       latest.Spec.Workload,
		Trigger:        latest.Spec.Trigger,
		Hint:           latest.Spec.Hint,
		FollowsTriage:  latest.Spec.FollowsTriage,
		TriggerCount:   latest.Status.TriggerCount,
		Steps:          recorded,
		StepCount:      latest.Status.StepCount,
		ToolCallCount:  latest.Status.ToolCallCount,
		Usage:          latest.Status.Usage,
		Findings:       nonNilFindings(latest.Status.Findings),
		Diagnosis:      latest.Status.Diagnosis,
		Proposal:       latest.Status.Proposal,
		Decision:       latest.Status.Decision,
		Execution:      latest.Status.Execution,
		Verification:   verificationOf(latest.Status),
		Narrative:      latest.Status.Narrative,
		FollowUpTriage: latest.Status.FollowUpTriage,
		Grafana:        h.grafana.links(&latest.Spec, &latest.Status, historyCreatedAt(latest), h.clock.Now()),
	}, nil
}

// detailFromLive renders the Triage resource as the dashboard's record.
func (h *Handler) detailFromLive(tr *v1alpha1.Triage) *DetailResponse {
	phase := tr.Status.Phase
	if phase == "" {
		phase = v1alpha1.PhasePending
	}
	return &DetailResponse{
		Name:           tr.Name,
		Namespace:      tr.Namespace,
		Phase:          phase,
		Source:         "live",
		CreatedAt:      liveCreatedAt(tr),
		StartedAt:      timePtr(tr.Status.StartedAt),
		TerminalAt:     timePtr(tr.Status.TerminalAt),
		Workload:       tr.Spec.Workload,
		Trigger:        tr.Spec.Trigger,
		Hint:           tr.Spec.Hint,
		FollowsTriage:  tr.Spec.FollowsTriage,
		TriggerCount:   tr.Status.TriggerCount,
		Steps:          nonNilSteps(tr.Status.Steps),
		StepCount:      tr.Status.StepCount,
		ToolCallCount:  tr.Status.ToolCallCount,
		Usage:          tr.Status.Usage,
		Findings:       nonNilFindings(tr.Status.Findings),
		Diagnosis:      tr.Status.Diagnosis,
		Proposal:       tr.Status.Proposal,
		Decision:       tr.Status.Decision,
		Execution:      tr.Status.Execution,
		Verification:   verificationOf(tr.Status),
		Narrative:      tr.Status.Narrative,
		FollowUpTriage: tr.Status.FollowUpTriage,
		Grafana:        h.grafana.links(&tr.Spec, &tr.Status, liveCreatedAt(tr), h.clock.Now()),
	}
}

// summarize renders one list row, preferring the live resource over its last history snapshot.
func (h *Handler) summarize(snapshot *history.Snapshot, tr *v1alpha1.Triage) TriageSummary {
	if tr != nil {
		phase := tr.Status.Phase
		if phase == "" {
			phase = v1alpha1.PhasePending
		}
		summary := TriageSummary{
			Name:           tr.Name,
			Namespace:      tr.Namespace,
			Phase:          phase,
			Workload:       tr.Spec.Workload,
			Trigger:        tr.Spec.Trigger,
			CreatedAt:      liveCreatedAt(tr),
			StartedAt:      timePtr(tr.Status.StartedAt),
			TerminalAt:     timePtr(tr.Status.TerminalAt),
			FollowUpTriage: tr.Status.FollowUpTriage,
			Source:         "live",
		}
		if tr.Status.Proposal != nil {
			summary.Action = tr.Status.Proposal.Action
		}
		return summary
	}
	summary := TriageSummary{
		Name:           snapshot.Triage,
		Namespace:      snapshot.Namespace,
		Phase:          snapshot.Phase,
		Workload:       snapshot.Spec.Workload,
		Trigger:        snapshot.Spec.Trigger,
		CreatedAt:      historyCreatedAt(snapshot),
		StartedAt:      timePtr(snapshot.Status.StartedAt),
		TerminalAt:     timePtr(snapshot.Status.TerminalAt),
		FollowUpTriage: snapshot.Status.FollowUpTriage,
		Source:         "history",
	}
	if snapshot.Status.Proposal != nil {
		summary.Action = snapshot.Status.Proposal.Action
	}
	return summary
}

// sortTime is when the Triage started, which orders the list. A Triage that the server has accepted but not yet
// started is ordered by its creation time instead, and a history-only row by its recorded creation time.
func sortTime(summary TriageSummary) time.Time {
	if summary.StartedAt != nil {
		return *summary.StartedAt
	}
	return summary.CreatedAt
}

// writeRecordError answers a record read: 404 when neither source knows the Triage, 500 otherwise.
func (h *Handler) writeRecordError(w http.ResponseWriter, ctx context.Context, err error) {
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	h.internal(w, ctx, err)
}

func (h *Handler) internal(w http.ResponseWriter, ctx context.Context, err error) {
	logf.FromContext(ctx).Error(err, "dashboard API request failed")
	writeError(w, http.StatusInternalServerError, "internal error")
}

// liveCreatedAt returns the Triage's creation time, falling back to when the server started it.
func liveCreatedAt(tr *v1alpha1.Triage) time.Time {
	if !tr.CreationTimestamp.IsZero() {
		return tr.CreationTimestamp.Time
	}
	if tr.Status.StartedAt != nil {
		return tr.Status.StartedAt.Time
	}
	return time.Time{}
}

// historyCreatedAt returns the recorded creation time, falling back to when the server started the Triage and
// then to when the snapshot was written, so a version 1 row still has an age.
func historyCreatedAt(snapshot *history.Snapshot) time.Time {
	if !snapshot.CreatedAt.IsZero() {
		return snapshot.CreatedAt
	}
	if snapshot.Status.StartedAt != nil {
		return snapshot.Status.StartedAt.Time
	}
	return snapshot.ObservedAt
}

// verificationOf renders the Verification timeline, which may have started and recorded no checks yet.
func verificationOf(status v1alpha1.TriageStatus) *Verification {
	if status.VerificationStartedAt == nil && len(status.VerificationChecks) == 0 {
		return nil
	}
	return &Verification{
		StartedAt: timePtr(status.VerificationStartedAt),
		Checks:    append([]v1alpha1.VerificationCheck{}, status.VerificationChecks...),
	}
}

func nonNilSteps(steps []v1alpha1.Step) []Step {
	if steps == nil {
		return []Step{}
	}
	return steps
}

func nonNilFindings(findings []v1alpha1.Finding) []Finding {
	if findings == nil {
		return []Finding{}
	}
	return findings
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	at := t.Time
	return &at
}

// knownPhase reports whether the value names a Phase.
func knownPhase(phase Phase) bool {
	switch phase {
	case v1alpha1.PhasePending, v1alpha1.PhaseInvestigating, v1alpha1.PhaseProposed, v1alpha1.PhaseApproved,
		v1alpha1.PhaseExecuting, v1alpha1.PhaseVerifying, v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered,
		v1alpha1.PhaseDiagnosed, v1alpha1.PhaseRejected, v1alpha1.PhaseExpired, v1alpha1.PhaseInvalid,
		v1alpha1.PhaseFailed:
		return true
	default:
		return false
	}
}

// knownTrigger reports whether the value names a Trigger source.
func knownTrigger(trigger v1alpha1.TriggerSource) bool {
	switch trigger {
	case v1alpha1.TriggerSourceAlertmanager, v1alpha1.TriggerSourceCLI, v1alpha1.TriggerSourceDashboard,
		v1alpha1.TriggerSourceFollowUp:
		return true
	default:
		return false
	}
}

// parseSince accepts either an RFC 3339 instant or a duration before now, such as 24h. An empty value means no
// lower bound.
func parseSince(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at, nil
	}
	window, err := time.ParseDuration(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be an RFC 3339 time or a duration such as 24h")
	}
	if window < 0 {
		return time.Time{}, fmt.Errorf("since must not be negative")
	}
	return now.Add(-window), nil
}

// writeEvent writes one Server-Sent Event and flushes it.
func writeEvent(w io.Writer, flusher http.Flusher, kind EventKind, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode the %s event: %w", kind, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes a JSON error body.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

package contextbundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/connectors/logs"
)

// Defaults bound every assembly even when the caller configures nothing.
const (
	// DefaultBudget is the wall-clock ceiling of one assembly.
	DefaultBudget = 25 * time.Second
	// DefaultLookBack is the default evidence window when the Trigger names none.
	DefaultLookBack = time.Hour
	// DefaultMaxBytes is the serialized bundle ceiling.
	DefaultMaxBytes = 2 << 20
	// DefaultMaxSectionBytes caps one component.
	DefaultMaxSectionBytes = 256 << 10
	// MaxEvents is the number of normalized Events a bundle keeps.
	MaxEvents = 100
	// MaxPods is the number of Pod states a bundle keeps.
	MaxPods = 50
	// MaxLogEntries is the number of log lines one Connector section keeps.
	MaxLogEntries = 500
	// MaxSeries is the number of metric series one Connector section keeps.
	MaxSeries = 100
	// deadlineNote explains a section that was never queried.
	deadlineNote = "the collection deadline ended before this Connector was queried"
)

// Request describes the Page whose evidence is assembled.
type Request struct {
	// Page is the Page name.
	Page string
	// PageUID is the Page UID. It makes the stored bundle unique per Page.
	PageUID string
	// Namespace is the Page namespace.
	Namespace string
	// Target is the Target the Page investigates.
	Target v1alpha1.TargetReference
	// Pod is the Pod the Trigger named, when it named one.
	Pod string
	// Since is the start of the look-back window. Zero uses DefaultLookBack.
	Since time.Time
}

// Store persists an encoded bundle once per Page.
type Store interface {
	// Put stores the bundle. It must keep the first bundle of a Page.
	Put(ctx context.Context, bundle BundleRecord) error
}

// BundleRecord is the encoded bundle handed to a Store.
type BundleRecord struct {
	// PageUID is the Page UID.
	PageUID string
	// Namespace is the Page namespace.
	Namespace string
	// Page is the Page name.
	Page string
	// Hash is the SHA-256 of the body.
	Hash string
	// Bytes is the body size.
	Bytes int64
	// Truncated reports whether a cap dropped evidence.
	Truncated bool
	// CollectedAt is when collection started.
	CollectedAt time.Time
	// Body is the encoded, sanitized bundle.
	Body string
}

// Assembler gathers the bounded, sanitized evidence of one Page. It is
// deterministic: the same evidence produces the same section order, caps, and
// hash. A Connector that fails degrades its section and never fails the Page.
type Assembler struct {
	// Reader reads the Target, its Pods, and Events. It is required.
	Reader client.Reader
	// Native is the mandatory native Kubernetes log collector.
	Native contract.LogConnector
	// Logs are the enabled external log Connectors, in catalogue order.
	Logs []contract.LogConnector
	// Metrics are the enabled metrics Connectors, in catalogue order.
	Metrics []contract.MetricsConnector
	// AlertStates are the enabled alert-state Connectors, in catalogue order.
	AlertStates []contract.AlertStateConnector
	// Sanitizer removes secret material. A nil value uses the built-in patterns.
	Sanitizer *Sanitizer
	// Store persists the encoded bundle. It is optional.
	Store Store
	// Clock defaults to the real clock.
	Clock clock.PassiveClock
	// Budget is the wall-clock ceiling. It defaults to DefaultBudget.
	Budget time.Duration
	// LookBack is the default evidence window. It defaults to DefaultLookBack.
	LookBack time.Duration
	// MaxBytes is the serialized bundle ceiling. It defaults to DefaultMaxBytes.
	MaxBytes int
	// MaxSectionBytes caps one component. It defaults to DefaultMaxSectionBytes.
	MaxSectionBytes int
}

// Assemble gathers the bundle, sanitizes every section, enforces the caps, and
// stores the encoded bundle once per Page.
func (a *Assembler) Assemble(ctx context.Context, request Request) (*Bundle, error) {
	if a.Reader == nil {
		return nil, errors.New("assemble a Context Bundle without a reader")
	}
	now := a.now()
	if request.Since.IsZero() {
		lookBack := a.LookBack
		if lookBack <= 0 {
			lookBack = DefaultLookBack
		}
		request.Since = now.Add(-lookBack)
	}
	budget := a.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	bundle := &Bundle{
		Version: Version, Page: request.Page, PageUID: request.PageUID,
		Namespace: request.Namespace, Target: request.Target, CollectedAt: now.UTC(),
	}
	sanitizer := a.Sanitizer
	if sanitizer == nil {
		sanitizer = &Sanitizer{}
	}
	a.addSection(ctx, bundle, sanitizer, "target", "", a.target(ctx, request))
	a.addSection(ctx, bundle, sanitizer, "pods", "", a.pods(ctx, request))
	a.addSection(ctx, bundle, sanitizer, "events", "", a.events(ctx, request))
	a.addSection(ctx, bundle, sanitizer, "native-logs", connectorName(a.Native),
		a.logsSection(ctx, a.Native, request))
	for _, connector := range a.Logs {
		a.addSection(ctx, bundle, sanitizer, "connector:logs:"+connector.Name(), connector.Name(),
			a.logsSection(ctx, connector, request))
	}
	for _, connector := range a.Metrics {
		a.addSection(ctx, bundle, sanitizer, "connector:metrics:"+connector.Name(), connector.Name(),
			a.metricsSection(ctx, connector, request))
	}
	for _, connector := range a.AlertStates {
		a.addSection(ctx, bundle, sanitizer, "connector:alertstate:"+connector.Name(), connector.Name(),
			a.alertStateSection(ctx, connector, request))
	}
	a.trim(bundle)
	if _, err := bundle.Encode(); err != nil {
		return nil, err
	}
	if a.Store != nil {
		encoded, err := bundle.Encode()
		if err != nil {
			return nil, err
		}
		record := BundleRecord{
			PageUID: bundle.PageUID, Namespace: bundle.Namespace, Page: bundle.Page,
			Hash: bundle.Hash, Bytes: int64(bundle.Bytes), Truncated: bundle.Truncated,
			CollectedAt: bundle.CollectedAt, Body: string(encoded),
		}
		if err := a.Store.Put(ctx, record); err != nil {
			return bundle, fmt.Errorf("store the Context Bundle: %w", err)
		}
	}
	return bundle, nil
}

// sectionResult is one component before it is sanitized and capped.
type sectionResult struct {
	payload    any
	notes      []string
	truncated  bool
	redactions int
}

// addSection marshals, sanitizes, and appends one component.
func (a *Assembler) addSection(
	ctx context.Context, bundle *Bundle, sanitizer *Sanitizer, name, connector string, result sectionResult,
) {
	if err := ctx.Err(); err != nil && result.payload == nil {
		result.notes = append(result.notes, "evidence collection reached its wall-clock deadline")
	}
	raw, err := json.Marshal(result.payload)
	if err != nil {
		section := Section{Name: name, Connector: connector,
			Notes: []string{"the component could not be encoded: " + err.Error()}, Truncated: true}
		bundle.Sections = append(bundle.Sections, section)
		bundle.Notes = append(bundle.Notes, name+" could not be encoded")
		bundle.Truncated = true
		return
	}
	sanitized, redactions := sanitizer.JSON(raw)
	section := Section{
		Name: name, Connector: connector, Payload: sanitized, Notes: result.notes,
		Truncated: result.truncated, Redactions: result.redactions + redactions,
	}
	limit := a.MaxSectionBytes
	if limit <= 0 {
		limit = DefaultMaxSectionBytes
	}
	if len(sanitized) > limit {
		section.Payload = json.RawMessage(`{"truncated":true}`)
		section.Notes = append(section.Notes, fmt.Sprintf(
			"the component was capped at %d bytes", limit))
		section.Truncated = true
	}
	section.Bytes = len(section.Payload)
	bundle.Sections = append(bundle.Sections, section)
	bundle.Redactions += section.Redactions
	if section.Truncated {
		bundle.Truncated = true
	}
}

// target reads the Target's identity and status. It keeps status rather than
// spec, because the bundle describes the fault, not the desired state.
func (a *Assembler) target(ctx context.Context, request Request) sectionResult {
	if request.Target.Name == "" {
		return sectionResult{notes: []string{"the Page names no Target"}}
	}
	kind := request.Target.Kind
	gvk := schema.FromAPIVersionAndKind(request.Target.APIVersion, string(kind))
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	key := client.ObjectKey{Namespace: request.Target.Namespace, Name: request.Target.Name}
	if kind.IsClusterScoped() {
		key = client.ObjectKey{Name: request.Target.Name}
	}
	if err := a.Reader.Get(ctx, key, object); err != nil {
		return sectionResult{notes: []string{"the Target could not be read: " + shortError(err)}, truncated: true}
	}
	status, _, _ := unstructured.NestedMap(object.Object, "status")
	return sectionResult{payload: map[string]any{
		"apiVersion": object.GetAPIVersion(),
		"kind":       object.GetKind(),
		"metadata": map[string]any{
			"name": object.GetName(), "namespace": object.GetNamespace(), "uid": string(object.GetUID()),
			"generation": object.GetGeneration(), "labels": object.GetLabels(),
		},
		"status": status,
	}}
}

// podState is one bounded Pod summary.
type podState struct {
	Name       string           `json:"name"`
	Node       string           `json:"node,omitempty"`
	Phase      string           `json:"phase"`
	Ready      bool             `json:"ready"`
	Restarts   int32            `json:"restarts"`
	CreatedAt  string           `json:"createdAt,omitempty"`
	Containers []containerState `json:"containers"`
}

// containerState is one bounded container summary.
type containerState struct {
	Name     string `json:"name"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int32  `json:"exitCode,omitempty"`
}

// pods summarizes the Target's Pod states.
func (a *Assembler) pods(ctx context.Context, request Request) sectionResult {
	pods, err := logs.PodsForTarget(ctx, a.Reader, request.Target)
	if err != nil {
		return sectionResult{notes: []string{"the Pod states could not be read: " + shortError(err)}, truncated: true}
	}
	slices.SortStableFunc(pods, func(a, b corev1.Pod) int {
		if a.Namespace != b.Namespace {
			return strings.Compare(a.Namespace, b.Namespace)
		}
		return strings.Compare(a.Name, b.Name)
	})
	result := sectionResult{}
	if len(pods) > MaxPods {
		pods = pods[:MaxPods]
		result.truncated = true
		result.notes = append(result.notes, fmt.Sprintf("the Pod list was capped at %d", MaxPods))
	}
	states := make([]podState, 0, len(pods))
	for i := range pods {
		states = append(states, summarizePod(&pods[i]))
	}
	return sectionResult{payload: states, notes: result.notes, truncated: result.truncated}
}

// summarizePod bounds one Pod.
func summarizePod(pod *corev1.Pod) podState {
	state := podState{
		Name: pod.Name, Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase),
	}
	if !pod.CreationTimestamp.IsZero() {
		state.CreatedAt = pod.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			state.Ready = condition.Status == corev1.ConditionTrue
		}
	}
	for _, container := range pod.Status.ContainerStatuses {
		state.Restarts += container.RestartCount
		summary := containerState{Name: container.Name, Ready: container.Ready, Restarts: container.RestartCount}
		switch {
		case container.State.Waiting != nil:
			summary.State = "waiting"
			summary.Reason = container.State.Waiting.Reason
			summary.Message = container.State.Waiting.Message
		case container.State.Terminated != nil:
			summary.State = "terminated"
			summary.Reason = container.State.Terminated.Reason
			summary.Message = container.State.Terminated.Message
			summary.ExitCode = container.State.Terminated.ExitCode
		case container.State.Running != nil:
			summary.State = "running"
		}
		state.Containers = append(state.Containers, summary)
	}
	return state
}

// eventState is one normalized Event, from either Event API.
type eventState struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Count     int32  `json:"count,omitempty"`
	FirstSeen string `json:"firstSeen,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
	Source    string `json:"source,omitempty"`
	APIGroup  string `json:"apiGroup"`
}

// events normalizes the Events of both Kubernetes Event APIs and keeps the
// ones that concern the Target.
func (a *Assembler) events(ctx context.Context, request Request) sectionResult {
	normalized := make([]eventState, 0, 32)
	var notes []string
	var coreEvents corev1.EventList
	options := []client.ListOption{client.InNamespace(request.Target.Namespace)}
	if err := a.Reader.List(ctx, &coreEvents, options...); err != nil {
		notes = append(notes, "core/v1 Events could not be read: "+shortError(err))
	} else {
		for i := range coreEvents.Items {
			event := &coreEvents.Items[i]
			if !concerns(event.InvolvedObject.Name, string(event.InvolvedObject.UID), request.Target) {
				continue
			}
			count := event.Count
			if count == 0 && event.Series != nil {
				count = event.Series.Count
			}
			normalized = append(normalized, eventState{
				Type: event.Type, Reason: event.Reason, Message: event.Message, Count: count,
				FirstSeen: formatEventTime(event.FirstTimestamp.Time), LastSeen: formatEventTime(event.LastTimestamp.Time),
				Source: event.Source.Component, APIGroup: "v1",
			})
		}
	}
	var newEvents eventsv1.EventList
	if err := a.Reader.List(ctx, &newEvents, options...); err != nil {
		notes = append(notes, "events.k8s.io/v1 Events could not be read: "+shortError(err))
	} else {
		for i := range newEvents.Items {
			event := &newEvents.Items[i]
			if !concerns(event.Regarding.Name, string(event.Regarding.UID), request.Target) {
				continue
			}
			count := event.DeprecatedCount
			lastSeen := event.EventTime.Time
			if event.Series != nil {
				if event.Series.Count > 0 {
					count = event.Series.Count
				}
				lastSeen = event.Series.LastObservedTime.Time
			}
			normalized = append(normalized, eventState{
				Type: event.Type, Reason: event.Reason, Message: event.Note, Count: count,
				FirstSeen: formatEventTime(event.EventTime.Time), LastSeen: formatEventTime(lastSeen),
				Source: event.ReportingController, APIGroup: "events.k8s.io",
			})
		}
	}
	slices.SortStableFunc(normalized, func(a, b eventState) int {
		if a.LastSeen != b.LastSeen {
			return strings.Compare(a.LastSeen, b.LastSeen)
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	result := sectionResult{notes: notes}
	if len(normalized) > MaxEvents {
		normalized = normalized[:MaxEvents]
		result.truncated = true
		result.notes = append(result.notes, fmt.Sprintf("the Event list was capped at %d", MaxEvents))
	}
	return sectionResult{payload: normalized, notes: result.notes, truncated: result.truncated}
}

// concerns reports whether an Event names the Target. A UID match wins; a name
// match covers an Event that predates the UID.
func concerns(name, uid string, target v1alpha1.TargetReference) bool {
	if name != target.Name {
		return false
	}
	return uid == "" || target.UID == "" || uid == target.UID
}

// formatEventTime renders a non-zero time in UTC.
func formatEventTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// logsSection queries one log Connector.
func (a *Assembler) logsSection(
	ctx context.Context, connector contract.LogConnector, request Request,
) sectionResult {
	if connector == nil {
		return sectionResult{notes: []string{"no native log Connector is configured"}, truncated: true}
	}
	if err := ctx.Err(); err != nil {
		return sectionResult{notes: []string{deadlineNote}}
	}
	page, err := connector.QueryLogs(ctx, contract.Query{
		Namespace: request.Target.Namespace, Subject: request.Target, Pod: request.Pod,
		Since: request.Since, Until: a.now(), Limit: MaxLogEntries,
	})
	if err != nil {
		return sectionResult{notes: []string{"Connector " + connector.Name() + " is unavailable: " + shortError(err)}}
	}
	entries := page.Entries
	truncated := page.Truncated
	if len(entries) > MaxLogEntries {
		entries = entries[:MaxLogEntries]
		truncated = true
	}
	type logLine struct {
		Time   string            `json:"time,omitempty"`
		Stream map[string]string `json:"stream,omitempty"`
		Line   string            `json:"line"`
	}
	lines := make([]logLine, 0, len(entries))
	for _, entry := range entries {
		line := logLine{Stream: entry.Stream, Line: entry.Line}
		if !entry.Timestamp.IsZero() {
			line.Time = entry.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		lines = append(lines, line)
	}
	notes := append([]string{}, page.Notes...)
	if truncated {
		notes = append(notes, fmt.Sprintf("the log page was capped at %d entries", MaxLogEntries))
	}
	return sectionResult{payload: lines, notes: notes, truncated: truncated}
}

// metricsSection queries one metrics Connector with its configured queries.
func (a *Assembler) metricsSection(
	ctx context.Context, connector contract.MetricsConnector, request Request,
) sectionResult {
	if err := ctx.Err(); err != nil {
		return sectionResult{notes: []string{deadlineNote}}
	}
	series, err := connector.QueryMetrics(ctx, contract.MetricQuery{
		Namespace: request.Target.Namespace, Subject: request.Target,
		Since: request.Since, Until: a.now(), Limit: MaxSeries,
	})
	if err != nil {
		return sectionResult{notes: []string{"Connector " + connector.Name() + " is unavailable: " + shortError(err)}}
	}
	truncated := len(series) > MaxSeries
	if truncated {
		series = series[:MaxSeries]
	}
	notes := []string{}
	if truncated {
		notes = append(notes, fmt.Sprintf("the series list was capped at %d", MaxSeries))
	}
	return sectionResult{payload: series, notes: notes, truncated: truncated}
}

// alertStateSection queries one alert-state Connector.
func (a *Assembler) alertStateSection(
	ctx context.Context, connector contract.AlertStateConnector, request Request,
) sectionResult {
	if err := ctx.Err(); err != nil {
		return sectionResult{notes: []string{deadlineNote}}
	}
	alerts, err := connector.QueryAlerts(ctx, contract.AlertQuery{
		Namespace: request.Target.Namespace, Subject: request.Target, Since: request.Since,
	})
	if err != nil {
		return sectionResult{notes: []string{"Connector " + connector.Name() + " is unavailable: " + shortError(err)}}
	}
	truncated := len(alerts) > MaxEvents
	if truncated {
		alerts = alerts[:MaxEvents]
	}
	notes := []string{}
	if truncated {
		notes = append(notes, fmt.Sprintf("the alert list was capped at %d", MaxEvents))
	}
	return sectionResult{payload: alerts, notes: notes, truncated: truncated}
}

// trim drops the least important sections until the serialized bundle fits the
// total ceiling, recording why each one was dropped.
func (a *Assembler) trim(bundle *Bundle) {
	limit := a.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	for len(bundle.Sections) > 0 {
		encoded, err := bundle.Encode()
		if err != nil || len(encoded) <= limit {
			return
		}
		dropped := bundle.Sections[len(bundle.Sections)-1]
		bundle.Sections = bundle.Sections[:len(bundle.Sections)-1]
		bundle.Notes = append(bundle.Notes,
			fmt.Sprintf("the %s section was dropped to keep the bundle within %d bytes", dropped.Name, limit))
		bundle.Truncated = true
	}
}

// now returns the assembler's clock time.
func (a *Assembler) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock.Now().UTC()
}

// connectorName returns a Connector's name, or an empty string.
func connectorName(connector contract.LogConnector) string {
	if connector == nil {
		return ""
	}
	return connector.Name()
}

// shortError bounds an error message for a degradation note.
func shortError(err error) string {
	message := err.Error()
	if len(message) > 200 {
		return message[:200]
	}
	return message
}

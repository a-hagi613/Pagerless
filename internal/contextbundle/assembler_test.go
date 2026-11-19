package contextbundle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Fixtures shared by the assembler tests.
const (
	assemblerTarget    = "checkout"
	assemblerPod       = "checkout-7d9c8f5b6d-2xk4p"
	assemblerNamespace = "shop"
)

// stubLogs is a log Connector with a fixed page or error.
type stubLogs struct {
	name  string
	page  contract.LogPage
	err   error
	calls int
}

// Name returns the Connector name.
func (s *stubLogs) Name() string { return s.name }

// QueryLogs returns the fixed page.
func (s *stubLogs) QueryLogs(context.Context, contract.Query) (contract.LogPage, error) {
	s.calls++
	return s.page, s.err
}

// stubMetrics is a metrics Connector with fixed series or an error.
type stubMetrics struct {
	name   string
	series []contract.MetricSeries
	err    error
}

// Name returns the Connector name.
func (s *stubMetrics) Name() string { return s.name }

// QueryMetrics returns the fixed series.
func (s *stubMetrics) QueryMetrics(context.Context, contract.MetricQuery) ([]contract.MetricSeries, error) {
	return s.series, s.err
}

// stubAlertState is an alert-state Connector with fixed alerts or an error.
type stubAlertState struct {
	name   string
	alerts []contract.AlertState
	err    error
}

// Name returns the Connector name.
func (s *stubAlertState) Name() string { return s.name }

// QueryAlerts returns the fixed alerts.
func (s *stubAlertState) QueryAlerts(context.Context, contract.AlertQuery) ([]contract.AlertState, error) {
	return s.alerts, s.err
}

// memoryStore keeps the first bundle of a Page.
type memoryStore struct {
	records []BundleRecord
}

// Put stores the first bundle only.
func (m *memoryStore) Put(_ context.Context, record BundleRecord) error {
	for _, existing := range m.records {
		if existing.PageUID == record.PageUID {
			return nil
		}
	}
	m.records = append(m.records, record)
	return nil
}

// testAssembler builds an assembler over a Deployment, one Pod, and two Events.
func testAssembler(t *testing.T) (*Assembler, *memoryStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, eventsv1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.November, 16, 12, 10, 0, 0, time.UTC)
	deployment := &appsv1.Deployment{
		Name: assemblerTarget, Namespace: assemblerNamespace, UID: "11111111-1111-1111-1111-111111111111",
		Labels: map[string]string{"app": assemblerTarget},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 3},
	}
	pod := &corev1.Pod{
		Name: assemblerPod, Namespace: assemblerNamespace, CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
		Labels: map[string]string{"app": assemblerTarget},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "checkout-7d9c8f5b6d",
		}},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: assemblerTarget, Ready: false, RestartCount: 3,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off 40s",
				}},
			}},
		},
	}
	event := &corev1.Event{
		Name: "checkout.1", Namespace: assemblerNamespace,
		InvolvedObject: corev1.ObjectReference{Name: assemblerTarget, Kind: "Deployment"},
		Type:           "Warning", Reason: "Unhealthy", Message: "Readiness probe failed",
		Count: 4, FirstTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)),
		LastTimestamp: metav1.NewTime(now.Add(-time.Minute)),
	}
	newEvent := &eventsv1.Event{
		Name: "checkout.2", Namespace: assemblerNamespace,
		Regarding: corev1.ObjectReference{Name: assemblerTarget, Kind: "Deployment"},
		Type:      "Warning", Reason: "FailedScheduling", Note: "0/3 nodes are available",
		EventTime: metav1.NewMicroTime(now.Add(-4 * time.Minute)),
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(deployment, pod, event, newEvent).Build()
	clock := clocktesting.NewFakeClock(now)
	store := &memoryStore{}
	assembler := &Assembler{
		Reader: fakeClient, Clock: clock, Store: store, Budget: 5 * time.Second,
		Native: &stubLogs{name: "native-k8s-logs", page: contract.LogPage{Entries: []contract.LogEntry{
			{
				Timestamp: now.Add(-time.Minute), Line: "starting checkout",
				Stream: map[string]string{"container": assemblerTarget},
			},
			{
				Timestamp: now.Add(-30 * time.Second), Line: "password=hunter2 connecting",
				Stream: map[string]string{"container": assemblerTarget},
			},
		}}},
		Logs: []contract.LogConnector{
			&stubLogs{name: "loki", page: contract.LogPage{Entries: []contract.LogEntry{{Line: "loki line"}}}},
			&stubLogs{name: "broken", err: errors.New("connection refused")},
		},
		Metrics: []contract.MetricsConnector{&stubMetrics{name: "prometheus", series: []contract.MetricSeries{
			{Labels: map[string]string{"pod": assemblerPod}, Points: []contract.MetricPoint{
				{Timestamp: now, Value: 0.92},
			}},
		}}},
		AlertStates: []contract.AlertStateConnector{&stubAlertState{name: "alertmanager", alerts: []contract.AlertState{
			{Name: "HighLatency", State: "active", Fingerprint: "abc"},
		}}},
	}
	sanitizer, err := NewSanitizer([]string{`hunter[0-9]`})
	if err != nil {
		t.Fatal(err)
	}
	assembler.Sanitizer = sanitizer
	return assembler, store
}

// testRequest returns the request the assembler tests use.
func testRequest() Request {
	return Request{
		Page: "checkout-abc123", PageUID: "22222222-2222-2222-2222-222222222222",
		Namespace: "pagerless",
		Target: v1alpha1.TargetReference{
			APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: assemblerNamespace,
			Name: assemblerTarget, UID: "11111111-1111-1111-1111-111111111111",
		},
		Pod: assemblerPod,
	}
}

func TestAssemblyIsDeterministicAndOrdered(t *testing.T) {
	t.Parallel()
	assembler, store := testAssembler(t)
	first, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"target", "pods", "events", "native-logs", "connector:logs:loki", "connector:logs:broken",
		"connector:metrics:prometheus", "connector:alertstate:alertmanager",
	}
	got := make([]string, 0, len(first.Sections))
	for _, section := range first.Sections {
		got = append(got, section.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sections = %v, want %v", got, want)
	}
	if first.Truncated {
		t.Errorf("bundle = %+v, want no truncation", first)
	}
	if first.Redactions < 1 {
		t.Errorf("redactions = %d, want the operator pattern and the assignment shape", first.Redactions)
	}
	if len(store.records) != 1 || store.records[0].Body == "" {
		t.Fatalf("stored records = %+v, want one encoded bundle", store.records)
	}

	second, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if second.Hash != first.Hash {
		t.Errorf("hash = %q, want the deterministic %q", second.Hash, first.Hash)
	}
	if second.Bytes != first.Bytes {
		t.Errorf("bytes = %d, want %d", second.Bytes, first.Bytes)
	}
}

func TestAFailingConnectorDegradesTheBundleWithoutFailingIt(t *testing.T) {
	t.Parallel()
	assembler, _ := testAssembler(t)
	bundle, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatalf("assembly failed on a Connector error: %v", err)
	}
	var broken *Section
	for index := range bundle.Sections {
		if bundle.Sections[index].Name == "connector:logs:broken" {
			broken = &bundle.Sections[index]
		}
	}
	if broken == nil {
		t.Fatal("the bundle lost the failing Connector's section")
	}
	joined := strings.Join(broken.Notes, " ")
	if !strings.Contains(joined, "broken") || !strings.Contains(joined, "unavailable") {
		t.Errorf("notes = %q, want a degradation note naming the Connector", joined)
	}
	summary := bundle.Summary()
	if len(summary.Notes) == 0 {
		t.Error("the Page summary lost the degradation note")
	}
}

func TestSanitizationRunsBeforeStorage(t *testing.T) {
	t.Parallel()
	assembler, store := testAssembler(t)
	bundle, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle.Sections[3].Payload), "hunter2") {
		t.Errorf("native logs retain the operator pattern:\n%s", bundle.Sections[3].Payload)
	}
	if strings.Contains(string(bundle.Sections[3].Payload), "starting checkout") == false {
		t.Error("sanitization dropped non-secret evidence")
	}
	if strings.Contains(store.records[0].Body, "hunter2") {
		t.Error("the stored bundle retains the secret")
	}
}

func TestTheTotalCeilingDropsSectionsAndRecordsWhy(t *testing.T) {
	t.Parallel()
	assembler, _ := testAssembler(t)
	assembler.MaxBytes = 1200
	bundle, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Truncated {
		t.Error("bundle = untruncated, want the total ceiling to drop evidence")
	}
	if len(bundle.Notes) == 0 {
		t.Error("bundle has no note explaining the dropped evidence")
	}
	if bundle.Bytes > assembler.MaxBytes {
		t.Errorf("bytes = %d, want at most %d", bundle.Bytes, assembler.MaxBytes)
	}
	encoded, err := bundle.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > assembler.MaxBytes {
		t.Errorf("encoded = %d bytes, want at most %d", len(encoded), assembler.MaxBytes)
	}
}

func TestTheEventSourceIsNormalizedAcrossBothAPIs(t *testing.T) {
	t.Parallel()
	assembler, _ := testAssembler(t)
	bundle, err := assembler.Assemble(t.Context(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	events := string(bundle.Sections[2].Payload)
	for _, want := range []string{"Unhealthy", "FailedScheduling", "events.k8s.io", `"apiGroup":"v1"`} {
		if !strings.Contains(events, want) {
			t.Errorf("events section lacks %q:\n%s", want, events)
		}
	}
}

func TestTheAssemblerRefusesWithoutAReader(t *testing.T) {
	t.Parallel()
	assembler := &Assembler{}
	if _, err := assembler.Assemble(t.Context(), testRequest()); err == nil {
		t.Fatal("Assemble succeeded without a reader")
	}
}

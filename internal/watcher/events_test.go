package watcher

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// testNow is the fixed clock the decoder tests read.
var testNow = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

// newEventsV1Event builds an Event of the dedicated events API.
func newEventsV1Event(reason, regardingKind, regardingName string) *eventsv1.Event {
	event := &eventsv1.Event{
		Namespace:           testNamespace,
		Name:                "checkout.1",
		UID:                 "e-1",
		ResourceVersion:     "1",
		EventTime:           metav1.NewMicroTime(testNow.Add(-time.Minute)),
		Type:                corev1.EventTypeWarning,
		Reason:              reason,
		Note:                "the controller reported a failure",
		Regarding:           corev1.ObjectReference{Kind: regardingKind, Namespace: testNamespace, Name: regardingName},
		ReportingController: "kubernetes.io/kubelet",
	}
	return event
}

// TestEventsV1SignalReadsRegarding is the release-blocking regression: the new
// API carries the object in `regarding`, not `involvedObject`, so a decoder that
// reads the legacy field finds nothing and a new-API crash loop pages nobody.
func TestEventsV1SignalReadsRegarding(t *testing.T) {
	t.Parallel()
	event := newEventsV1Event(testEventReason, testKindPod, "checkout-7d9f-abc12")
	signal, ok := EventsV1Signal(event, testNow)
	if !ok {
		t.Fatal("a Warning Event on the new API produced no Signal")
	}
	if signal.Reference.Kind != testKindPod || signal.Reference.Name != "checkout-7d9f-abc12" {
		t.Fatalf("signal reference = %+v, want the Pod named by regarding", signal.Reference)
	}
	if signal.Class != ReasonPodCrashLoop {
		t.Fatalf("class = %s, want %s", signal.Class, ReasonPodCrashLoop)
	}
	// The legacy field does not exist on this type; decoding an empty object
	// reference is what a copy-pasted core/v1 decoder effectively does.
	legacy := referenceOf(corev1.ObjectReference{}, event.Namespace)
	if legacy.Kind != "" || legacy.Name != "" {
		t.Fatalf("legacy reference = %+v, want empty", legacy)
	}
}

func TestCoreEventSignalReadsInvolvedObject(t *testing.T) {
	t.Parallel()
	event := &corev1.Event{
		Namespace:      testNamespace,
		Name:           "checkout.1",
		Type:           corev1.EventTypeWarning,
		Reason:         "FailedMount",
		Message:        "the volume is not attached",
		InvolvedObject: corev1.ObjectReference{Kind: testKindPod, Namespace: testNamespace, Name: testPodName},
		FirstTimestamp: metav1.NewTime(testNow.Add(-2 * time.Minute)),
		Count:          7,
	}
	signal, ok := CoreEventSignal(event, testNow)
	if !ok {
		t.Fatal("a Warning Event on the legacy API produced no Signal")
	}
	if signal.Reference.Kind != testKindPod || signal.Reference.Name != testPodName {
		t.Fatalf("signal reference = %+v, want the Pod named by involvedObject", signal.Reference)
	}
	if signal.Class != ReasonStorageFailedMount {
		t.Fatalf("class = %s, want %s", signal.Class, ReasonStorageFailedMount)
	}
	if signal.Count != 7 {
		t.Fatalf("count = %d, want the Event count 7", signal.Count)
	}
}

func TestNormalEventsAreNotIngested(t *testing.T) {
	t.Parallel()
	legacy := &corev1.Event{
		Type:           corev1.EventTypeNormal,
		Reason:         "Started",
		InvolvedObject: corev1.ObjectReference{Kind: testKindPod, Namespace: testNamespace, Name: testPodName},
	}
	if _, ok := CoreEventSignal(legacy, testNow); ok {
		t.Fatal("a Normal Event on the legacy API must not produce a Signal")
	}
	fresh := newEventsV1Event(testEventReason, testKindPod, testPodName)
	fresh.Type = corev1.EventTypeNormal
	if _, ok := EventsV1Signal(fresh, testNow); ok {
		t.Fatal("a Normal Event on the new API must not produce a Signal")
	}
}

func TestUnknownReasonIsNotIngested(t *testing.T) {
	t.Parallel()
	event := newEventsV1Event("SomethingNew", testKindPod, testPodName)
	if _, ok := EventsV1Signal(event, testNow); ok {
		t.Fatal("an unknown reason must not be classified")
	}
}

func TestEventsV1SignalFallsBackToRelated(t *testing.T) {
	t.Parallel()
	event := newEventsV1Event("FailedScheduling", "HorizontalPodAutoscaler", testDeployment)
	event.Related = &corev1.ObjectReference{Kind: testKindDeployment, Namespace: testNamespace, Name: testDeployment}
	signal, ok := EventsV1Signal(event, testNow)
	if !ok {
		t.Fatal("a related reference to a watched kind must produce a Signal")
	}
	if signal.Reference.Kind != testKindDeployment || signal.Reference.Name != testDeployment {
		t.Fatalf("reference = %+v, want the related Deployment", signal.Reference)
	}
	if signal.ResolvedFrom != "related" {
		t.Fatalf("resolvedFrom = %q, want related", signal.ResolvedFrom)
	}
}

func TestEventsV1SignalFallsBackToTheEventNamespace(t *testing.T) {
	t.Parallel()
	event := newEventsV1Event(testEventReason, testKindPod, testPodName)
	event.Regarding.Namespace = ""
	signal, ok := EventsV1Signal(event, testNow)
	if !ok {
		t.Fatal("a namespaced regarding without a namespace must still resolve")
	}
	if signal.Reference.Namespace != testNamespace {
		t.Fatalf("namespace = %q, want the Event's namespace", signal.Reference.Namespace)
	}
	node := newEventsV1Event("NodeNotReady", testKindNode, testNodeName)
	node.Regarding.Namespace = ""
	signal, ok = EventsV1Signal(node, testNow)
	if !ok {
		t.Fatal("a Node Event must resolve")
	}
	if signal.Reference.Namespace != "" {
		t.Fatalf("node namespace = %q, want empty", signal.Reference.Namespace)
	}
}

func TestEventsV1SignalReadsTheSeriesCount(t *testing.T) {
	t.Parallel()
	event := newEventsV1Event(testEventReason, testKindPod, testPodName)
	event.Series = &eventsv1.EventSeries{Count: 412, LastObservedTime: metav1.NewMicroTime(testNow)}
	if signal, _ := EventsV1Signal(event, testNow); signal.Count != 412 {
		t.Fatalf("count = %d, want the series count 412", signal.Count)
	}
	event.Series = nil
	event.DeprecatedCount = 9
	if signal, _ := EventsV1Signal(event, testNow); signal.Count != 9 {
		t.Fatalf("count = %d, want the deprecated count 9", signal.Count)
	}
}

func TestRevisionCacheProcessesEachVersionOnce(t *testing.T) {
	t.Parallel()
	cache := newRevisionCache(2)
	if !cache.firstTime(types.UID("e-1"), "1") {
		t.Fatal("the first version must be processed")
	}
	if cache.firstTime(types.UID("e-1"), "1") {
		t.Fatal("the same version must not be processed twice")
	}
	if !cache.firstTime(types.UID("e-1"), "2") {
		t.Fatal("a new version must be processed")
	}
	if !cache.firstTime(types.UID("e-2"), "1") {
		t.Fatal("a new Event must be processed")
	}
	// The cache is bounded, so the first Event was evicted.
	if !cache.firstTime(types.UID("e-1"), "1") {
		t.Fatal("an evicted Event is processed again rather than growing the cache without bound")
	}
}

func TestOccurrenceDelta(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		previous, count, want int64
	}{
		"first observation":               {previous: 0, count: 0, want: 1},
		"series grew":                     {previous: 410, count: 412, want: 2},
		"same series count":               {previous: 412, count: 412, want: 1},
		"count went backwards":            {previous: 100, count: 1, want: 1},
		"aggregated count":                {previous: 0, count: 412, want: 412},
		"unknown count is an observation": {previous: 0, count: 0, want: 1},
	}
	for name, testCase := range cases {
		if got := occurrenceDelta(testCase.previous, testCase.count); got != testCase.want {
			t.Errorf("%s: delta = %d, want %d", name, got, testCase.want)
		}
	}
}

// TestSignalNormalizationClampsTheClock proves that a node's future timestamp
// cannot extend a settle window, because normalized clamps it to now.
func TestSignalNormalizationClampsTheClock(t *testing.T) {
	t.Parallel()
	future := Signal{FirstSeen: testNow.Add(time.Hour)}
	if got := future.normalized(testNow).FirstSeen; !got.Equal(testNow) {
		t.Fatalf("firstSeen = %s, want it clamped to now", got)
	}
	old := Signal{FirstSeen: testNow.Add(-3 * time.Hour)}
	if got := old.normalized(testNow).FirstSeen; !got.Equal(testNow.Add(-time.Hour)) {
		t.Fatalf("firstSeen = %s, want it clamped to an hour ago", got)
	}
}

func TestEventReferenceFallsBackOnlyForNamespacedKinds(t *testing.T) {
	t.Parallel()
	namespaced := referenceOf(corev1.ObjectReference{Kind: testKindDeployment, Name: testDeployment}, testNamespace)
	if namespaced.Namespace != testNamespace {
		t.Fatalf("namespace = %q, want the Event namespace", namespaced.Namespace)
	}
	clusterScoped := referenceOf(corev1.ObjectReference{Kind: testKindNode, Name: testNodeName}, testNamespace)
	if clusterScoped.Namespace != "" {
		t.Fatalf("namespace = %q, want empty for a Node", clusterScoped.Namespace)
	}
}

// TestWatchableKinds documents which raw references the Event ingester is
// willing to canonicalize.
func TestWatchableKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{testKindPod, testKindDeployment, testKindStatefulSet, "DaemonSet",
		"Job", testKindCronJob, "PersistentVolumeClaim", testKindNode, "ReplicaSet"} {
		if !watchable(kind) {
			t.Errorf("kind %q must be watchable", kind)
		}
	}
	for _, kind := range []string{"", "Secret", "PodTemplate", "NodeMetrics"} {
		if watchable(kind) {
			t.Errorf("kind %q must not be watchable", kind)
		}
	}
}

// TestSignalTriggerCarriesEvidenceNotIdentity proves the Trigger keeps the raw
// reason and message as evidence while the Fingerprint ignores them.
func TestSignalTriggerCarriesEvidence(t *testing.T) {
	t.Parallel()
	signal := Signal{
		Class:  ReasonPodCrashLoop,
		Reason: testEventReason,
		Note:   "Back-off restarting failed container",
		Pod:    testPodName,
		Source: v1alpha1.TriggerSourceEvent,
	}
	trigger := signal.Trigger()
	if trigger.Source != v1alpha1.TriggerSourceEvent || trigger.Reason != testEventReason ||
		trigger.Pod != testPodName {
		t.Fatalf("trigger = %+v, want the raw evidence", trigger)
	}
	long := Signal{Note: string(make([]byte, maxTriggerMessage+100))}
	if got := long.Trigger().Message; len(got) != maxTriggerMessage {
		t.Fatalf("message length = %d, want %d", len(got), maxTriggerMessage)
	}
}

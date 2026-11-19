package watcher

import (
	"context"
	"errors"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// EventSource names the Event API a reconciler reads.
type EventSource string

const (
	// SourceCoreV1 is the legacy Event API, which most kubelets and control-plane
	// components still emit through.
	SourceCoreV1 EventSource = "core/v1"
	// SourceEventsV1 is the dedicated events API, whose object reference is
	// `regarding`, not `involvedObject`.
	SourceEventsV1 EventSource = "events.k8s.io/v1"
)

// maxProcessedEvents bounds the resource-version memory, so a long-lived server
// cannot grow it without limit.
const maxProcessedEvents = 4096

// CoreEventSignal projects a core/v1 Event onto a Signal. It reports false for an
// Event that must not be ingested: anything that is not a Warning, or whose
// reason the catalog does not classify.
func CoreEventSignal(event *corev1.Event, now time.Time) (Signal, bool) {
	if event.Type != corev1.EventTypeWarning {
		return Signal{}, false
	}
	class, ok := ClassifyEventReason(event.Reason)
	if !ok {
		return Signal{}, false
	}
	reference := referenceOf(event.InvolvedObject, event.Namespace)
	return Signal{
		Reference: reference,
		Class:     class,
		Note:      event.Message,
		Pod:       podName(reference),
		Source:    v1alpha1.TriggerSourceEvent,
		Reason:    event.Reason,
		FirstSeen: eventFirstSeen(event.FirstTimestamp.Time, event.EventTime.Time, now),
		Count:     int64(event.Count),
	}, true
}

// EventsV1Signal projects an events.k8s.io/v1 Event onto a Signal. The object
// reference is `regarding`; a decoder that reads `involvedObject` on this type
// produces an empty reference and silently matches nothing, which is the
// regression the suite guards against.
//
// When `regarding` names a kind the watcher does not watch and `related` names
// one it does, the signal carries the related object and records that it
// resolved from it. That covers Horizontal Pod Autoscaler and admission Events.
func EventsV1Signal(event *eventsv1.Event, now time.Time) (Signal, bool) {
	if event.Type != corev1.EventTypeWarning {
		return Signal{}, false
	}
	class, ok := ClassifyEventReason(event.Reason)
	if !ok {
		return Signal{}, false
	}
	reference := referenceOf(event.Regarding, event.Namespace)
	resolvedFrom := ""
	if !watchable(reference.Kind) {
		if event.Related == nil {
			return Signal{}, false
		}
		related := referenceOf(*event.Related, event.Namespace)
		if !watchable(related.Kind) {
			return Signal{}, false
		}
		reference, resolvedFrom = related, "related"
	}
	var observed time.Time
	if event.Series != nil {
		observed = event.Series.LastObservedTime.Time
	}
	signal := Signal{
		Reference:    reference,
		Class:        class,
		Note:         event.Note,
		Pod:          podName(reference),
		Source:       v1alpha1.TriggerSourceEvent,
		Reason:       event.Reason,
		FirstSeen:    eventFirstSeen(event.EventTime.Time, observed, now),
		Count:        eventCount(event),
		ResolvedFrom: resolvedFrom,
	}
	return signal, true
}

// eventCount returns the repetition count of an events.k8s.io/v1 Event: the
// series count when the API server aggregated it, otherwise the deprecated
// count a migrated emitter still sets.
func eventCount(event *eventsv1.Event) int64 {
	if event.Series != nil && event.Series.Count > 0 {
		return int64(event.Series.Count)
	}
	return int64(event.DeprecatedCount)
}

// eventFirstSeen returns the earliest usable timestamp of an Event. An Event's
// clock belongs to another node, so normalized clamps it; a zero time falls back
// to now.
func eventFirstSeen(first, second, now time.Time) time.Time {
	switch {
	case !first.IsZero():
		return first
	case !second.IsZero():
		return second
	default:
		return now
	}
}

// referenceOf projects an ObjectReference onto a Reference. A namespaced kind
// falls back to the Event's own namespace, because a third-party emitter
// occasionally omits it; a Node keeps its namespace empty. The namespace is never
// inferred from the message.
func referenceOf(object corev1.ObjectReference, eventNamespace string) Reference {
	namespace := object.Namespace
	if namespace == "" && v1alpha1.TargetKind(object.Kind) != v1alpha1.TargetKindNode {
		namespace = eventNamespace
	}
	return Reference{
		APIVersion: object.APIVersion,
		Kind:       object.Kind,
		Namespace:  namespace,
		Name:       object.Name,
		UID:        string(object.UID),
	}
}

// podName returns the Pod an Event names directly, when it names one.
func podName(reference Reference) string {
	if v1alpha1.TargetKind(reference.Kind) == v1alpha1.TargetKindPod {
		return reference.Name
	}
	return ""
}

// revisionCache remembers the last processed resource version of each Event, so
// one Event object produces one Signal per version even though a resync
// redelivers it. Entries are evicted oldest-first past the bound.
type revisionCache struct {
	mu    sync.Mutex
	seen  map[types.UID]string
	order []types.UID
	limit int
}

// newRevisionCache returns a cache that holds at most limit Events.
func newRevisionCache(limit int) *revisionCache {
	if limit <= 0 {
		limit = maxProcessedEvents
	}
	return &revisionCache{seen: make(map[types.UID]string), limit: limit}
}

// firstTime reports whether this is the first time uid is seen at version.
func (c *revisionCache) firstTime(uid types.UID, version string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[uid] == version && version != "" {
		return false
	}
	if _, known := c.seen[uid]; !known {
		c.order = append(c.order, uid)
	}
	c.seen[uid] = version
	for len(c.order) > c.limit {
		evicted := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, evicted)
	}
	return true
}

// EventReconciler ingests one Event API. Two instances run, one per API, because
// binding only one misses the other emitter population.
type EventReconciler struct {
	// Client reads Events through the cache.
	Client client.Client
	// Pipeline decides what the Signal opens, joins, or suppresses.
	Pipeline *Pipeline
	// Source is the Event API this reconciler reads.
	Source EventSource
	// processed remembers the last resource version of each Event.
	processed *revisionCache
}

// setup registers the reconciler for its Event type.
func (r *EventReconciler) setup(mgr ctrl.Manager) error {
	if r.processed == nil {
		r.processed = newRevisionCache(maxProcessedEvents)
	}
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("event-" + string(r.Source)).
		WithOptions(controller.Options{SkipNameValidation: new(true)})
	switch r.Source {
	case SourceCoreV1:
		builder = builder.For(&corev1.Event{})
	case SourceEventsV1:
		builder = builder.For(&eventsv1.Event{})
	default:
		return errors.New("unknown Event source " + string(r.Source))
	}
	return builder.Complete(r)
}

// Reconcile ingests one Event. A malformed Event is dropped, never fatal: the
// controller must not crash on another emitter's bug.
func (r *EventReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	switch r.Source {
	case SourceCoreV1:
		var event corev1.Event
		if err := r.Client.Get(ctx, req.NamespacedName, &event); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if !r.processed.firstTime(event.UID, event.ResourceVersion) {
			return ctrl.Result{}, nil
		}
		signal, ok := CoreEventSignal(&event, r.Pipeline.now())
		if !ok {
			return ctrl.Result{}, nil
		}
		if err := r.Pipeline.HandleEvent(ctx, signal); err != nil && !ignorable(err) {
			log.Error(err, "ingest a core/v1 Event", "event", req.NamespacedName)
		}
	case SourceEventsV1:
		var event eventsv1.Event
		if err := r.Client.Get(ctx, req.NamespacedName, &event); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if !r.processed.firstTime(event.UID, event.ResourceVersion) {
			return ctrl.Result{}, nil
		}
		signal, ok := EventsV1Signal(&event, r.Pipeline.now())
		if !ok {
			return ctrl.Result{}, nil
		}
		if err := r.Pipeline.HandleEvent(ctx, signal); err != nil && !ignorable(err) {
			log.Error(err, "ingest an events.k8s.io/v1 Event", "event", req.NamespacedName)
		}
	}
	return ctrl.Result{}, nil
}

// ignorable reports whether a resolver error is an expected outcome rather than
// a failure: a signal on an unsupported object is dropped, not retried.
func ignorable(err error) bool {
	return errors.Is(err, ErrNoSubject) || errors.Is(err, ErrIgnored) || apierrors.IsNotFound(err)
}

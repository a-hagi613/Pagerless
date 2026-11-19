// Package dashboardapi serves the dashboard's read-only HTTP API and its embedded single-page application. It
// reads Pages through the same controller-runtime client that the rest of pagerless-server uses, so the
// dashboard inherits the server's RBAC boundaries and never writes anything (ADR-0001, ADR-0013).
package dashboardapi

import "sync"

// EventKind is the kind of Server-Sent Event that the dashboard API sends.
type EventKind string

const (
	// EventSnapshot carries the complete Page record at the moment the stream opened.
	EventSnapshot EventKind = "snapshot"
	// EventStep carries one step event that the server recorded.
	EventStep EventKind = "step"
	// EventPhase carries a Phase change.
	EventPhase EventKind = "phase"
	// EventEnd closes a stream whose Page has reached a terminal Phase.
	EventEnd EventKind = "end"
)

// Event is one message on a Page's event stream.
type Event struct {
	Kind EventKind
	Data any
}

// StepEvent is the data of an EventStep.
type StepEvent struct {
	Step Step `json:"step"`
	// Finding is the Finding the step carried, when it carried one.
	Finding *Finding `json:"finding,omitempty"`
	// StepCount is the Page's total number of steps after this one.
	StepCount int32 `json:"stepCount"`
}

// PhaseEvent is the data of an EventPhase and an EventEnd.
type PhaseEvent struct {
	Phase Phase `json:"phase"`
	// Terminal reports whether the Phase is terminal, after which the stream ends.
	Terminal bool `json:"terminal"`
}

// subscriberBuffer is how many events a slow stream may fall behind before the broker drops one. Publishing never
// blocks the status write path, and a client that falls this far behind reconnects and re-reads the snapshot.
const subscriberBuffer = 128

// Broker fans step and Phase events out to the dashboard streams of one Page. It is safe for concurrent use.
type Broker struct {
	mu          sync.Mutex
	next        int
	subscribers map[int]*subscription
}

type subscription struct {
	page   string
	events chan Event
}

// NewBroker returns a Broker with no subscribers.
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[int]*subscription)}
}

// Subscribe returns the channel of events for one Page and a function that ends the subscription. The channel
// is closed when the subscription ends.
func (b *Broker) Subscribe(page string) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	sub := &subscription{page: page, events: make(chan Event, subscriberBuffer)}
	b.subscribers[id] = sub
	return sub.events, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if current, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(current.events)
		}
	}
}

// Publish sends an event to every stream of the Page. It never blocks: when a stream's buffer is full the event
// is dropped for that stream, and the client recovers the record by reconnecting.
func (b *Broker) Publish(page string, kind EventKind, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subscribers {
		if sub.page != page {
			continue
		}
		select {
		case sub.events <- Event{Kind: kind, Data: data}:
		default:
		}
	}
}

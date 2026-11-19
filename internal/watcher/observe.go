package watcher

import (
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// maxTriggerMessage is the longest message a Page Trigger stores.
const maxTriggerMessage = 1024

// Signal is one reason class observed at one instant. The raw Event or status
// field has already been mapped to a catalog class, which is what keeps a note
// from ever becoming identity.
type Signal struct {
	// Reference names the raw object that carried the signal, usually a Pod.
	Reference Reference
	// Class is the catalog constant.
	Class ReasonClass
	// Scope is the container name, the condition type, or empty.
	Scope string
	// Note is the human-readable text at first observation. It is evidence.
	Note string
	// Pod is the Pod that raised the signal, when one did.
	Pod string
	// Source is Event or Condition.
	Source v1alpha1.TriggerSource
	// Reason is the raw Kubernetes reason, kept as evidence.
	Reason string
	// FirstSeen is when the signal was first observed, from the signal's own
	// timestamp when it has one.
	FirstSeen time.Time
	// Count is the Event series or deprecated count. It is zero for status signals.
	Count int64
	// Immediate bypasses the settle window for a signal that has already lasted long
	// enough by construction, such as a third restart with a terminated container.
	Immediate bool
	// ResolvedFrom names the raw field that produced the signal's object when it
	// was not the Event's primary reference, such as "related".
	ResolvedFrom string
}

// normalized clamps the signal's clock and note. Event timestamps come from
// another node, so a future timestamp cannot extend a settle window.
func (s Signal) normalized(now time.Time) Signal {
	if s.FirstSeen.IsZero() || s.FirstSeen.After(now) {
		s.FirstSeen = now
	}
	if s.FirstSeen.Before(now.Add(-time.Hour)) {
		s.FirstSeen = now.Add(-time.Hour)
	}
	s.Note = truncateMessage(s.Note)
	return s
}

// Trigger is the Page Trigger a Signal produces.
func (s Signal) Trigger() v1alpha1.Trigger {
	return v1alpha1.Trigger{
		Source:  s.Source,
		Reason:  truncateReason(s.Reason),
		Message: truncateMessage(s.Note),
		Pod:     truncateReason(s.Pod),
	}
}

// truncateMessage bounds a message to what a Trigger stores.
func truncateMessage(message string) string {
	if len(message) <= maxTriggerMessage {
		return message
	}
	return message[:maxTriggerMessage]
}

// truncateReason bounds a reason or name to what a Trigger stores.
func truncateReason(reason string) string {
	const maxReason = 128
	if len(reason) <= maxReason {
		return reason
	}
	return reason[:maxReason]
}

// occurrenceDelta returns how many observations a new Event count adds. A count
// that goes backwards is a re-used object or a clock skew, and counts as one.
func occurrenceDelta(previous, count int64) int64 {
	if count <= 0 {
		return 1
	}
	delta := count - previous
	if delta <= 0 {
		return 1
	}
	return delta
}

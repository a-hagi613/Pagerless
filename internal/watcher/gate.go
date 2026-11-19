package watcher

import (
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Defaults for the gate. They are the specified noise-control values, so a zero
// Gate behaves like the documented one.
const (
	// DefaultAggregationWindow is the window inside which repeats of one
	// Fingerprint count as occurrences rather than as new Triggers.
	DefaultAggregationWindow = 10 * time.Minute
	// DefaultBurst is the per-Subject occurrence ceiling inside one window.
	DefaultBurst = 25
	// DefaultEscalateWarningAt is the occurrence count that raises severity one level.
	DefaultEscalateWarningAt = 10
	// DefaultEscalateCriticalAt is the occurrence count that caps severity at critical.
	DefaultEscalateCriticalAt = 25
	// DefaultAvailabilityFloorPercent is the share of a Subject's desired
	// replicas that may be unavailable without raising its severity. It is what
	// stops one noisy replica of a large fleet from spending model budget on a
	// Page that no file edit can improve.
	DefaultAvailabilityFloorPercent = 20
)

// Availability is a Subject's replica health, which decides the availability
// severity modifier. A Node, a claim, or a standalone Pod is desired=1 when it
// exists.
type Availability struct {
	// Desired is the number of replicas the Subject wants.
	Desired int32
	// Ready is the number of replicas that are ready.
	Ready int32
}

// Gate applies the deterministic severity, settle, and burst rules. It holds no
// state: every decision is a function of the signal and the Subject's health.
type Gate struct {
	// MinSeverity is the lowest severity that may open a Page. It defaults to warning.
	MinSeverity v1alpha1.Severity
	// AggregationWindow bounds when a repeat counts as a new Trigger.
	AggregationWindow time.Duration
	// Burst is the occurrence ceiling per Subject inside one window.
	Burst int64
	// EscalateWarningAt and EscalateCriticalAt are the occurrence and suppression
	// counts that raise severity.
	EscalateWarningAt  int64
	EscalateCriticalAt int64
	// AvailabilityFloorPercent is the percentage of desired replicas that must be
	// unavailable before availability raises severity. It defaults to 20.
	AvailabilityFloorPercent int32
}

// DefaultGate returns the specified gate.
func DefaultGate() Gate {
	return Gate{
		MinSeverity:              v1alpha1.SeverityWarning,
		AggregationWindow:        DefaultAggregationWindow,
		Burst:                    DefaultBurst,
		EscalateWarningAt:        DefaultEscalateWarningAt,
		EscalateCriticalAt:       DefaultEscalateCriticalAt,
		AvailabilityFloorPercent: DefaultAvailabilityFloorPercent,
	}
}

// withDefaults fills every unset field.
func (g Gate) withDefaults() Gate {
	if g.MinSeverity == "" {
		g.MinSeverity = v1alpha1.SeverityWarning
	}
	if g.AggregationWindow <= 0 {
		g.AggregationWindow = DefaultAggregationWindow
	}
	if g.Burst <= 0 {
		g.Burst = DefaultBurst
	}
	if g.EscalateWarningAt <= 0 {
		g.EscalateWarningAt = DefaultEscalateWarningAt
	}
	if g.EscalateCriticalAt <= 0 {
		g.EscalateCriticalAt = DefaultEscalateCriticalAt
	}
	if g.AvailabilityFloorPercent <= 0 || g.AvailabilityFloorPercent > 100 {
		g.AvailabilityFloorPercent = DefaultAvailabilityFloorPercent
	}
	return g
}

// Pageable reports whether a severity may open a Page.
func (g Gate) Pageable(severity v1alpha1.Severity) bool {
	return severityRank(severity) >= severityRank(g.withDefaults().MinSeverity)
}

// Severity computes the Page severity: the maximum of the catalog base, the
// availability modifier, the occurrence escalation, and the suppression
// escalation. It never exceeds critical and never drops below the base.
func (g Gate) Severity(entry Entry, availability Availability, occurrences, suppressed int64) v1alpha1.Severity {
	gate := g.withDefaults()
	severity := maximum(entry.Base, AvailabilitySeverity(availability, gate.AvailabilityFloorPercent))
	severity = maximum(severity, escalate(occurrences, gate.EscalateWarningAt, gate.EscalateCriticalAt))
	severity = maximum(severity, escalate(suppressed, gate.EscalateWarningAt, gate.EscalateCriticalAt))
	return severity
}

// Settled reports whether a signal has persisted for its catalog settle window.
// The clock is measured from the signal's own first observation, not from the
// watcher's receipt, so a restart does not restart the clock.
func (g Gate) Settled(entry Entry, firstSeen, now time.Time, immediate bool) bool {
	if immediate {
		return true
	}
	return !now.Before(firstSeen.Add(entry.Settle))
}

// WithinBurst reports whether an occurrence count is still inside the per-Subject
// burst ceiling. Beyond it the occurrences are counted but cannot escalate.
func (g Gate) WithinBurst(occurrences int64) bool {
	return occurrences <= g.withDefaults().Burst
}

// Aggregated reports whether a repeat belongs to the aggregation window that a
// previous Trigger opened, in which case it increments occurrences without
// emitting another Trigger.
func (g Gate) Aggregated(lastTrigger, now time.Time) bool {
	if lastTrigger.IsZero() {
		return false
	}
	return now.Sub(lastTrigger) < g.withDefaults().AggregationWindow
}

// AvailabilitySeverity applies the availability modifier alone. The floor is
// the percentage of desired replicas that may be unavailable before the
// modifier reaches warning: below it the fleet is healthy enough that a single
// noisy replica must not page. A fleet with no ready replica is critical
// whatever the floor says, and a Subject without desired replicas is info.
func AvailabilitySeverity(availability Availability, floorPercent int32) v1alpha1.Severity {
	if floorPercent <= 0 || floorPercent > 100 {
		floorPercent = DefaultAvailabilityFloorPercent
	}
	unavailable := availability.Desired - availability.Ready
	switch {
	case unavailable <= 0:
		return v1alpha1.SeverityInfo
	case availability.Ready <= 0:
		return v1alpha1.SeverityCritical
	case int64(unavailable)*100 < int64(availability.Desired)*int64(floorPercent):
		return v1alpha1.SeverityInfo
	default:
		return v1alpha1.SeverityWarning
	}
}

// escalate raises severity one level at warningAt and caps it at critical at
// criticalAt. A count below warningAt changes nothing.
func escalate(count, warningAt, criticalAt int64) v1alpha1.Severity {
	switch {
	case count >= criticalAt:
		return v1alpha1.SeverityCritical
	case count >= warningAt:
		return v1alpha1.SeverityWarning
	default:
		return v1alpha1.SeverityInfo
	}
}

// maximum returns the higher severity.
func maximum(a, b v1alpha1.Severity) v1alpha1.Severity {
	if severityRank(a) >= severityRank(b) {
		return a
	}
	return b
}

// severityRank orders severities.
func severityRank(severity v1alpha1.Severity) int {
	switch severity {
	case v1alpha1.SeverityCritical:
		return 2
	case v1alpha1.SeverityWarning:
		return 1
	case v1alpha1.SeverityInfo:
		return 0
	default:
		return 0
	}
}

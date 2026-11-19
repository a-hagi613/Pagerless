package watcher

import (
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestAvailabilitySeverity(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		availability Availability
		want         v1alpha1.Severity
	}{
		"no replicas ready":   {availability: Availability{Desired: 3, Ready: 0}, want: v1alpha1.SeverityCritical},
		"some replicas ready": {availability: Availability{Desired: 3, Ready: 2}, want: v1alpha1.SeverityWarning},
		"one replica of ten":  {availability: Availability{Desired: 10, Ready: 9}, want: v1alpha1.SeverityInfo},
		"all replicas ready":  {availability: Availability{Desired: 3, Ready: 3}, want: v1alpha1.SeverityInfo},
		"scaled to zero":      {availability: Availability{Desired: 0, Ready: 0}, want: v1alpha1.SeverityInfo},
	}
	for name, testCase := range cases {
		if got := AvailabilitySeverity(testCase.availability, DefaultAvailabilityFloorPercent); got != testCase.want {
			t.Errorf("%s: AvailabilitySeverity = %s, want %s", name, got, testCase.want)
		}
	}
}

// TestAvailabilityFloorProvesTheNoisyReplicaRule documents the budget guardrail:
// one unready replica of a ten-replica fleet is below the floor, so it does not
// raise severity, while a third of the fleet being down does.
func TestAvailabilityFloorProvesTheNoisyReplicaRule(t *testing.T) {
	t.Parallel()
	noisy := Entry{Class: ReasonPodCrashLoop, Base: v1alpha1.SeverityInfo}
	gate := DefaultGate()
	if got := gate.Severity(noisy, Availability{Desired: 10, Ready: 9}, 1, 0); got != v1alpha1.SeverityInfo {
		t.Errorf("one of ten replicas severity = %s, want info", got)
	}
	if got := gate.Severity(noisy, Availability{Desired: 3, Ready: 2}, 1, 0); got != v1alpha1.SeverityWarning {
		t.Errorf("two of three replicas severity = %s, want warning", got)
	}
	strict := Gate{AvailabilityFloorPercent: 5}
	if got := strict.Severity(noisy, Availability{Desired: 10, Ready: 9}, 1, 0); got != v1alpha1.SeverityWarning {
		t.Errorf("a five percent floor severity = %s, want warning", got)
	}
}

func TestSeverityIsTheMaximumOfBaseAvailabilityAndEscalation(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	info := Entry{Class: ReasonPodCrashLoop, Base: v1alpha1.SeverityInfo}
	// One noisy replica of a healthy fleet does not page.
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 3}, 1, 0); got != v1alpha1.SeverityInfo {
		t.Errorf("healthy fleet severity = %s, want info", got)
	}
	// The whole fleet down is critical whatever the base says.
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 0}, 1, 0); got != v1alpha1.SeverityCritical {
		t.Errorf("fleet down severity = %s, want critical", got)
	}
	// Ten occurrences raise an info signal to warning; twenty-five cap it at critical.
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 3}, 10, 0); got != v1alpha1.SeverityWarning {
		t.Errorf("ten occurrences severity = %s, want warning", got)
	}
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 3}, 25, 0); got != v1alpha1.SeverityCritical {
		t.Errorf("twenty-five occurrences severity = %s, want critical", got)
	}
	// Suppression escalates the Page that eventually opens, by the same thresholds.
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 3}, 0, 10); got != v1alpha1.SeverityWarning {
		t.Errorf("ten suppressions severity = %s, want warning", got)
	}
	if got := gate.Severity(info, Availability{Desired: 3, Ready: 3}, 0, 25); got != v1alpha1.SeverityCritical {
		t.Errorf("twenty-five suppressions severity = %s, want critical", got)
	}
}

func TestSeverityNeverDecreasesBelowBase(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	critical := Entry{Class: ReasonNodeNotReady, Base: v1alpha1.SeverityCritical}
	if got := gate.Severity(critical, Availability{Desired: 1, Ready: 1}, 1, 0); got != v1alpha1.SeverityCritical {
		t.Errorf("severity = %s, want critical", got)
	}
}

func TestPageableUsesTheMinimumSeverity(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	if gate.Pageable(v1alpha1.SeverityInfo) {
		t.Error("info must not open a Page at the default minimum")
	}
	if !gate.Pageable(v1alpha1.SeverityWarning) || !gate.Pageable(v1alpha1.SeverityCritical) {
		t.Error("warning and critical must open a Page")
	}
	infoGate := Gate{MinSeverity: v1alpha1.SeverityInfo}
	if !infoGate.Pageable(v1alpha1.SeverityInfo) {
		t.Error("info must open a Page when the minimum is info")
	}
}

func TestSettledUsesTheSignalClock(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	entry, _ := LookupEntry(ReasonSchedulingFailed)
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	if gate.Settled(entry, now.Add(-time.Minute), now, false) {
		t.Error("a signal one minute old has not settled at two minutes")
	}
	if !gate.Settled(entry, now.Add(-entry.Settle), now, false) {
		t.Error("a signal at its settle boundary must be eligible")
	}
	if !gate.Settled(entry, now, now, true) {
		t.Error("an immediate signal bypasses the settle window")
	}
}

func TestAggregatedUsesTheWindow(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	if gate.Aggregated(time.Time{}, now) {
		t.Error("a Fingerprint with no Trigger is not aggregated")
	}
	if !gate.Aggregated(now.Add(-DefaultAggregationWindow+time.Second), now) {
		t.Error("a repeat inside the window is aggregated")
	}
	if gate.Aggregated(now.Add(-DefaultAggregationWindow), now) {
		t.Error("a repeat at the window boundary is a new Trigger")
	}
}

func TestWithinBurst(t *testing.T) {
	t.Parallel()
	gate := DefaultGate()
	if !gate.WithinBurst(DefaultBurst) {
		t.Error("the burst ceiling itself is inside the burst")
	}
	if gate.WithinBurst(DefaultBurst + 1) {
		t.Error("one occurrence past the ceiling is outside the burst")
	}
}

package watcher

import (
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestCatalogCoversEveryClassOnce(t *testing.T) {
	t.Parallel()
	classes := Classes()
	seen := make(map[ReasonClass]struct{}, len(classes))
	for _, class := range classes {
		if _, duplicate := seen[class]; duplicate {
			t.Errorf("class %q is listed twice", class)
		}
		seen[class] = struct{}{}
		entry, ok := LookupEntry(class)
		if !ok {
			t.Errorf("class %q is listed but has no catalog entry", class)
			continue
		}
		if entry.Class != class {
			t.Errorf("class %q has entry %q", class, entry.Class)
		}
		if entry.Base != v1alpha1.SeverityInfo && entry.Base != v1alpha1.SeverityWarning &&
			entry.Base != v1alpha1.SeverityCritical {
			t.Errorf("class %q has no base severity", class)
		}
		if entry.Settle < 0 {
			t.Errorf("class %q has a negative settle window", class)
		}
	}
	if len(seen) != len(catalog) {
		t.Errorf("the catalog has %d classes and Classes lists %d", len(catalog), len(seen))
	}
}

func TestLookupEntryIgnoresUnknownClasses(t *testing.T) {
	t.Parallel()
	if _, ok := LookupEntry(ReasonClass("pod.invented")); ok {
		t.Fatal("an unknown reason class must not resolve")
	}
}

func TestClassifyEventReason(t *testing.T) {
	t.Parallel()
	known := map[string]ReasonClass{
		"FailedScheduling":       ReasonSchedulingFailed,
		"FailedMount":            ReasonStorageFailedMount,
		"FailedAttachVolume":     ReasonStorageFailedAttach,
		"FailedCreatePodSandBox": ReasonStorageFailedSandbox,
		"ProvisioningFailed":     ReasonStoragePVCProvisioning,
		"Unhealthy":              ReasonHealthUnhealthy,
		"FailedToPullImage":      ReasonPodImagePull,
		testEventReason:          ReasonPodCrashLoop,
		"Evicted":                ReasonPodEvicted,
		"OOMKilling":             ReasonPodOOMKilled,
		"NodeNotReady":           ReasonNodeNotReady,
	}
	for reason, want := range known {
		got, ok := ClassifyEventReason(reason)
		if !ok || got != want {
			t.Errorf("ClassifyEventReason(%q) = %q, %v; want %q, true", reason, got, ok, want)
		}
	}
	for _, reason := range []string{"", "Scheduled", "Pulled", "Started", "Killing", "Failed"} {
		if class, ok := ClassifyEventReason(reason); ok {
			t.Errorf("ClassifyEventReason(%q) = %q, want it ignored", reason, class)
		}
	}
}

func TestCatalogSettleWindows(t *testing.T) {
	t.Parallel()
	cases := map[ReasonClass]time.Duration{
		ReasonPodCrashLoop:            time.Minute,
		ReasonPodOOMKilled:            0,
		ReasonPodImagePull:            30 * time.Second,
		ReasonSchedulingFailed:        2 * time.Minute,
		ReasonStoragePVCProvisioning:  5 * time.Minute,
		ReasonRolloutProgressDeadline: 0,
		ReasonRolloutStatefulSetStuck: 10 * time.Minute,
		ReasonNodeNotReady:            2 * time.Minute,
		ReasonNodeMemoryPressure:      5 * time.Minute,
	}
	for class, want := range cases {
		entry, ok := LookupEntry(class)
		if !ok {
			t.Fatalf("class %q has no entry", class)
		}
		if entry.Settle != want {
			t.Errorf("class %q settle = %s, want %s", class, entry.Settle, want)
		}
	}
}

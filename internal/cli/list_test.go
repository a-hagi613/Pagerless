package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestParsePhaseAcceptsTheWholePhaseEnum(t *testing.T) {
	want := []v1alpha1.Phase{
		v1alpha1.PhasePending,
		v1alpha1.PhaseInvestigating,
		v1alpha1.PhaseProposed,
		v1alpha1.PhaseApproved,
		v1alpha1.PhasePullRequestOpen,
		v1alpha1.PhaseVerifying,
		v1alpha1.PhaseRecovered,
		v1alpha1.PhaseNotRecovered,
		v1alpha1.PhaseDiagnosed,
		v1alpha1.PhaseRejected,
		v1alpha1.PhaseExpired,
		v1alpha1.PhaseInvalid,
		v1alpha1.PhaseSuperseded,
		v1alpha1.PhaseFailed,
	}
	if !slices.Equal(pagePhases, want) {
		t.Fatalf("pagePhases = %v, want exactly the CRD's Phase enum %v", pagePhases, want)
	}
	for _, phase := range want {
		got, err := parsePhase(string(phase))
		if err != nil {
			t.Errorf("parsePhase(%q): %v", phase, err)
			continue
		}
		if got != phase {
			t.Errorf("parsePhase(%q) = %q", phase, got)
		}
	}
}

func TestParsePhaseTreatsEmptyAsNoFilter(t *testing.T) {
	got, err := parsePhase("  ")
	if err != nil {
		t.Fatalf("parsePhase(empty): %v", err)
	}
	if got != "" {
		t.Errorf("parsePhase(empty) = %q, want no filter", got)
	}
}

func TestParsePhaseRejectsAnUnknownPhaseAndNamesTheAcceptedSet(t *testing.T) {
	_, err := parsePhase("proposed")
	if err == nil {
		t.Fatal("parsePhase(\"proposed\") succeeded, want a usage error")
	}
	if code := ExitCode(err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(err.Error(), `unknown phase "proposed"`) {
		t.Errorf("error = %q, want it to name the rejected value", err)
	}
	for _, phase := range pagePhases {
		if !strings.Contains(err.Error(), string(phase)) {
			t.Errorf("error = %q, want it to name the accepted phase %q", err, phase)
		}
	}
}

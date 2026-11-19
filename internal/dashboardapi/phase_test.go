package dashboardapi

import (
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// TestKnownPhaseCoversTheWholeEnum guards the ?phase= filter against falling
// behind the Page CRD's enum.
func TestKnownPhaseCoversTheWholeEnum(t *testing.T) {
	phases := []v1alpha1.Phase{
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
	for _, phase := range phases {
		if !knownPhase(phase) {
			t.Errorf("knownPhase(%q) = false, want true", phase)
		}
	}
	if knownPhase(v1alpha1.Phase("Proposedd")) {
		t.Error(`knownPhase("Proposedd") = true, want false`)
	}
}

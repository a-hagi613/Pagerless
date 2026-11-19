package verification_test

import (
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// mergeTime is the moment the Pull Request merged.
var mergeTime = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// healthy is an observation of a healthy Target at the offset after the merge.
func healthy(offset time.Duration) verification.Observation {
	return verification.Observation{
		Now: mergeTime.Add(offset), GitOpsObserved: true, Available: true,
		ReadyReplicas: 3, DesiredReplicas: 3,
	}
}

func TestPostMergeRecoversAfterTheHealthyWindow(t *testing.T) {
	t.Parallel()
	postMerge := &verification.PostMerge{}
	state := postMerge.Observe(verification.PostMergeState{}, healthy(time.Minute))
	if state.Phase != verification.PostMergeObserving || state.Outcome != v1alpha1.PhaseVerifying {
		t.Fatalf("state = %+v, want Verifying inside the window", state)
	}
	for _, offset := range []time.Duration{2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 6 * time.Minute} {
		state = postMerge.Observe(state, healthy(offset))
	}
	if !state.Recovered() || state.Outcome != v1alpha1.PhaseRecovered {
		t.Fatalf("state = %+v, want Recovered after five healthy minutes", state)
	}
	for _, check := range state.Checks {
		if !check.Passed && check.Name != v1alpha1.CheckHealthyWindow {
			t.Errorf("check %s = %+v, want it to pass", check.Name, check)
		}
	}
}

func TestPostMergeWaitsForGitOps(t *testing.T) {
	t.Parallel()
	postMerge := &verification.PostMerge{}
	unobserved := healthy(time.Minute)
	unobserved.GitOpsObserved = false
	state := postMerge.Observe(verification.PostMergeState{}, unobserved)
	if state.Phase != verification.PostMergeWaitingForGitOps {
		t.Fatalf("phase = %s, want WaitingForGitOps", state.Phase)
	}
	state = postMerge.Observe(state, healthy(2*time.Minute))
	if state.Phase != verification.PostMergeObserving {
		t.Fatalf("phase = %s, want Observing once the controller observes the merge", state.Phase)
	}
}

func TestPostMergeFailsWhenGitOpsNeverObserves(t *testing.T) {
	t.Parallel()
	postMerge := &verification.PostMerge{}
	unobserved := healthy(0)
	unobserved.GitOpsObserved = false
	state := postMerge.Observe(verification.PostMergeState{}, unobserved)
	unobserved.Now = mergeTime.Add(verification.GitOpsSyncWindow)
	state = postMerge.Observe(state, unobserved)
	if state.Outcome != v1alpha1.PhaseNotRecovered || !strings.Contains(state.Reason, "did not observe the merge") {
		t.Fatalf("state = %+v, want NotRecovered with the sync reason", state)
	}
}

func TestPostMergeFailsOnAnUnhealthyTarget(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*verification.Observation){
		"unavailable": func(o *verification.Observation) { o.Available = false },
		"progressing": func(o *verification.Observation) { o.Progressing = true },
		"not ready":   func(o *verification.Observation) { o.ReadyReplicas = 2 },
		"warning": func(o *verification.Observation) {
			o.NewWarningEvents = []string{"BackOff restarting failed container"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			postMerge := &verification.PostMerge{}
			observation := healthy(time.Minute)
			mutate(&observation)
			state := postMerge.Observe(verification.PostMergeState{}, observation)
			if state.Outcome != v1alpha1.PhaseNotRecovered || state.Reason == "" {
				t.Fatalf("state = %+v, want NotRecovered with a reason", state)
			}
		})
	}
}

func TestPostMergeIsDeterministic(t *testing.T) {
	t.Parallel()
	postMerge := &verification.PostMerge{}
	first := postMerge.Observe(verification.PostMergeState{}, healthy(time.Minute))
	second := postMerge.Observe(verification.PostMergeState{}, healthy(time.Minute))
	if len(first.Checks) != len(second.Checks) {
		t.Fatalf("the same observation produced different checks: %+v and %+v", first.Checks, second.Checks)
	}
	for index := range first.Checks {
		if first.Checks[index] != second.Checks[index] {
			t.Errorf("check %d = %+v, want %+v", index, second.Checks[index], first.Checks[index])
		}
	}
}

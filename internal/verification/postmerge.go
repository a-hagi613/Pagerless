package verification

import (
	"fmt"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// PostMergeWindow is the healthy observation window after a merge. The Target
// must stay healthy for the whole window, so a transient recovery does not end
// the Page as Recovered.
const PostMergeWindow = 5 * time.Minute

// GitOpsSyncWindow bounds how long the Page waits for the GitOps controller to
// observe the merge. A controller that has not observed it by then is a
// NotRecovered outcome with that reason, not an unbounded wait.
const GitOpsSyncWindow = 10 * time.Minute

// PostMergePhase is the stage of post-merge Verification.
type PostMergePhase string

const (
	// PostMergeWaitingForGitOps means the merge is recorded and the GitOps
	// controller has not observed it yet.
	PostMergeWaitingForGitOps PostMergePhase = "WaitingForGitOps"
	// PostMergeObserving means the GitOps controller observed the merge and the
	// healthy window is running.
	PostMergeObserving PostMergePhase = "Observing"
	// PostMergeDecided means Verification reached Recovered or NotRecovered.
	PostMergeDecided PostMergePhase = "Decided"
)

// Observation is one reading of the Target after the merge.
type Observation struct {
	// Now is when the reading was taken.
	Now time.Time
	// GitOpsObserved reports that the GitOps controller has observed the merge
	// commit and reports the Target at that revision.
	GitOpsObserved bool
	// Available reports that the Target's availability condition is True.
	Available bool
	// Progressing reports that the Target's rollout is still progressing.
	Progressing bool
	// ReadyReplicas and DesiredReplicas are the Target's replica counts.
	ReadyReplicas, DesiredReplicas int32
	// NewWarningEvents are the BackOff and Failed Warning Events for the
	// Target's pods since the merge.
	NewWarningEvents []string
}

// PostMergeState is the state of post-merge Verification for one Page.
type PostMergeState struct {
	// Phase is the stage.
	Phase PostMergePhase
	// StartedAt is when Verification started: the moment the merge was observed.
	StartedAt time.Time
	// HealthySince is when the Target first became healthy in the window. It is
	// zero while the Target is unhealthy.
	HealthySince time.Time
	// Checks are the deterministic checks recorded on the Page.
	Checks []v1alpha1.VerificationCheck
	// Outcome is Verifying while the window runs, then Recovered or NotRecovered.
	Outcome v1alpha1.Phase
	// Reason explains a NotRecovered outcome.
	Reason string
}

// PostMerge evaluates the Target after a merge. Every decision is a pure
// function of the observations, so the same readings always decide the same
// outcome.
type PostMerge struct {
	// Window is the healthy observation window. It defaults to PostMergeWindow.
	Window time.Duration
	// SyncWindow is how long the GitOps controller may take to observe the merge.
	// It defaults to GitOpsSyncWindow.
	SyncWindow time.Duration
}

// Observe advances the state with one reading.
func (p *PostMerge) Observe(state PostMergeState, observation Observation) PostMergeState {
	window, syncWindow := p.Window, p.SyncWindow
	if window <= 0 {
		window = PostMergeWindow
	}
	if syncWindow <= 0 {
		syncWindow = GitOpsSyncWindow
	}
	if state.Phase == "" {
		state.Phase = PostMergeWaitingForGitOps
		state.StartedAt = observation.Now
		state.Outcome = v1alpha1.PhaseVerifying
	}
	if state.Phase == PostMergeDecided {
		return state
	}
	state.Checks = []v1alpha1.VerificationCheck{
		{Name: v1alpha1.CheckGitOpsObserved, Passed: observation.GitOpsObserved,
			Message: gitOpsMessage(observation.GitOpsObserved)},
		{Name: v1alpha1.CheckRolloutAvailable, Passed: observation.Available && !observation.Progressing,
			Message: rolloutMessage(observation)},
		{Name: v1alpha1.CheckReadyReplicas,
			Passed:  observation.DesiredReplicas > 0 && observation.ReadyReplicas == observation.DesiredReplicas,
			Message: replicaMessage(observation)},
		{Name: v1alpha1.CheckNoNewWarningEvents, Passed: len(observation.NewWarningEvents) == 0,
			Message: eventMessage(observation.NewWarningEvents)},
	}
	if !observation.GitOpsObserved {
		if observation.Now.Sub(state.StartedAt) >= syncWindow {
			return p.decide(state, false, fmt.Sprintf(
				"the GitOps controller did not observe the merge within %s", syncWindow))
		}
		state.Phase = PostMergeWaitingForGitOps
		return state
	}
	state.Phase = PostMergeObserving
	healthy := observation.Available && !observation.Progressing &&
		observation.DesiredReplicas > 0 && observation.ReadyReplicas == observation.DesiredReplicas &&
		len(observation.NewWarningEvents) == 0
	if !healthy {
		return p.decide(state, false, unhealthyReason(observation))
	}
	if state.HealthySince.IsZero() {
		state.HealthySince = observation.Now
	}
	if observation.Now.Sub(state.HealthySince) >= window {
		return p.decide(state, true, "")
	}
	state.Checks = append(state.Checks, v1alpha1.VerificationCheck{
		Name: v1alpha1.CheckHealthyWindow, Passed: false,
		Message: fmt.Sprintf("the Target has been healthy for %s of %s",
			observation.Now.Sub(state.HealthySince).Round(time.Second), window),
	})
	return state
}

// decide ends Verification.
func (p *PostMerge) decide(state PostMergeState, recovered bool, reason string) PostMergeState {
	state.Phase = PostMergeDecided
	state.Reason = reason
	state.Checks = append(state.Checks, v1alpha1.VerificationCheck{
		Name: v1alpha1.CheckHealthyWindow, Passed: recovered, Message: windowMessage(recovered, reason),
	})
	if recovered {
		state.Outcome = v1alpha1.PhaseRecovered
	} else {
		state.Outcome = v1alpha1.PhaseNotRecovered
	}
	return state
}

// Recovered reports whether the state is a Recovered outcome.
func (s PostMergeState) Recovered() bool { return s.Outcome == v1alpha1.PhaseRecovered }

func gitOpsMessage(observed bool) string {
	if observed {
		return "the GitOps controller observed the merge"
	}
	return "the GitOps controller has not observed the merge"
}

func rolloutMessage(observation Observation) string {
	switch {
	case !observation.Available:
		return "the Target is not available"
	case observation.Progressing:
		return "the Target's rollout is still progressing"
	default:
		return "the Target is available and its rollout is complete"
	}
}

func replicaMessage(observation Observation) string {
	return fmt.Sprintf("%d of %d replicas are ready", observation.ReadyReplicas, observation.DesiredReplicas)
}

func eventMessage(events []string) string {
	if len(events) == 0 {
		return "no new BackOff or Failed Warning Event was recorded for the Target's pods"
	}
	return fmt.Sprintf("%d new BackOff or Failed Warning Events: %s", len(events), events[0])
}

func unhealthyReason(observation Observation) string {
	switch {
	case !observation.Available:
		return "the Target is not available"
	case observation.Progressing:
		return "the Target's rollout is still progressing"
	case observation.DesiredReplicas == 0:
		return "the Target has no desired replicas"
	case observation.ReadyReplicas != observation.DesiredReplicas:
		return replicaMessage(observation)
	default:
		return eventMessage(observation.NewWarningEvents)
	}
}

func windowMessage(recovered bool, reason string) string {
	if recovered {
		return "the Target stayed healthy for the whole observation window"
	}
	return reason
}

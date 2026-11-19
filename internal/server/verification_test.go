package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const alertFingerprint = "3f1a9c0d2b7e4a65"

// The Argo CD sync and health statuses that the tests move between, and the termination reason of a container
// that exceeded its memory limit.
const (
	outOfSync = "OutOfSync"
	synced    = "Synced"
	healthy   = "Healthy"
	oomKilled = "OOMKilled"
)

// alertTrigger is the Trigger of an opted-in Alertmanager alert.
var alertTrigger = v1alpha1.Trigger{
	Source: v1alpha1.TriggerSourceAlertmanager, Pod: "checkout-7d9f8-x2k4q", AlertFingerprint: alertFingerprint,
}

// humanTrigger is the Trigger of a human request through the CLI.
var humanTrigger = v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI}

// podLabels are the labels that the Target's Deployment selects its pods by.
func podLabels(name string) map[string]string { return map[string]string{"app": name} }

// settle gives the server's reconcilers time to act, for a test that asserts that nothing changed.
func settle() { time.Sleep(600 * time.Millisecond) }

// eventually polls until the condition holds.
func eventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	err := wait.PollUntilContextTimeout(t.Context(), 50*time.Millisecond, 30*time.Second, true,
		func(context.Context) (bool, error) { return condition(), nil })
	if err != nil {
		t.Fatalf("waiting until %s: %v", description, err)
	}
}

// merged returns a Page for the checkout Target, started by the Trigger, whose approved pull request has
// merged, with the merge commit that Argo CD has not yet synced.
func merged(t *testing.T, env *testenv.Env, trigger v1alpha1.Trigger) (*v1alpha1.Page, string) {
	t.Helper()
	deploy(t, env, target{name: checkout})
	candidate := testenv.NewPage(checkout)
	candidate.Spec.Trigger = trigger
	created := env.CreatePage(t, candidate)
	agent := env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), patchReport("app", nil, quantities("memory", "512Mi")))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	commit := env.GitHub.CompleteChecks(t, 1)
	tr := env.WaitFor(t, proposed.Name, "the merge commit is recorded", func(tr *v1alpha1.Page) bool {
		return tr.Status.PullRequest != nil && tr.Status.PullRequest.MergeCommit != ""
	})
	return tr, commit
}

// verifying returns the Page once Argo CD has synced the merge commit and Verification has begun. The
// Deployment's rollout completes first, as it does while Argo CD's sync converges.
func verifying(t *testing.T, env *testenv.Env, tr *v1alpha1.Page, commit string) *v1alpha1.Page {
	t.Helper()
	env.MarkDeploymentAvailable(t, sandboxNamespace, tr.Spec.Target.Name)
	env.ArgoCD.Sync(commit)
	return env.WaitForPhase(t, tr.Name, v1alpha1.PhaseVerifying)
}

// checkResult returns the recorded result of the check, and fails the test if there is none.
func checkResult(t *testing.T, tr *v1alpha1.Page, name string) v1alpha1.VerificationCheck {
	t.Helper()
	for _, c := range tr.Status.VerificationChecks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("Page %s records no result for check %s: %+v", tr.Name, name, tr.Status.VerificationChecks)
	return v1alpha1.VerificationCheck{}
}

// requireChecks checks which of the checks passed.
func requireChecks(t *testing.T, tr *v1alpha1.Page, want map[string]bool) {
	t.Helper()
	if len(tr.Status.VerificationChecks) != len(want) {
		t.Errorf("checks = %+v, want exactly %d results", tr.Status.VerificationChecks, len(want))
	}
	for name, passed := range want {
		if got := checkResult(t, tr, name); got.Passed != passed || got.Message == "" {
			t.Errorf("check %s = %+v, want passed=%t with a message", name, got, passed)
		}
	}
}

func TestVerificationStartsOnlyOnceTheMergeCommitIsSyncedAndHealthy(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	env.MarkDeploymentAvailable(t, sandboxNamespace, checkout)
	base := env.Sandbox.Rev(commit + "^")

	t.Run("a merged commit that Argo CD has not synced", func(t *testing.T) {
		for _, status := range []testenv.ArgoCDStatus{
			{Sync: outOfSync, Revision: base, Health: healthy},
			{Sync: synced, Revision: base, Health: healthy},
			{Sync: outOfSync, Revision: commit, Health: healthy},
			{Sync: synced, Revision: commit, Health: "Progressing"},
			{Sync: synced, Revision: commit, Health: "Degraded"},
			{Sync: synced, Revision: commit, Health: ""},
			{Sync: "Unknown", Revision: "", Health: healthy},
		} {
			env.ArgoCD.Set(testenv.ArgoCDApplication, status)
			before := len(env.ArgoCD.Requests())
			eventually(t, "Argo CD is asked again", func() bool { return len(env.ArgoCD.Requests()) >= before+3 })
			if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhasePullRequestOpen {
				t.Fatalf("Argo CD reports %+v, but the phase is %s, want Executing", status, got.Status.Phase)
			}
		}
	})
	t.Run("Argo CD errors do not start it either", func(t *testing.T) {
		env.ArgoCD.FailNext(1_000_000)
		env.ArgoCD.Sync(commit)
		before := len(env.ArgoCD.Requests())
		eventually(t, "Argo CD is asked again", func() bool { return len(env.ArgoCD.Requests()) >= before+3 })
		if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhasePullRequestOpen {
			t.Fatalf("phase = %s while Argo CD fails, want Executing", got.Status.Phase)
		}
		env.ArgoCD.FailNext(0)
	})
	t.Run("synced and healthy at the merge commit", func(t *testing.T) {
		got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseVerifying)
		if got.Status.VerificationStartedAt == nil || !got.Status.VerificationStartedAt.Time.Equal(env.Clock.Now()) {
			t.Errorf("verification started at %v, want the clock's time %v", got.Status.VerificationStartedAt, env.Clock.Now())
		}
		requireChecks(t, got, map[string]bool{v1alpha1.CheckArgoCDSynced: true})
		if message := checkResult(t, got, v1alpha1.CheckArgoCDSynced).Message; !strings.Contains(message, commit) {
			t.Errorf("message = %q, want it to name the merge commit %s", message, commit)
		}
	})
}

func TestVerificationDoesNotStartBeforeThePullRequestMerges(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, target{name: checkout})
	proposed := proposedPage(t, env, checkout)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	// Argo CD is healthy at the commit the pull request was based on, as it is before any change.
	env.MarkDeploymentAvailable(t, sandboxNamespace, checkout)
	env.ArgoCD.Sync(proposed.Status.Proposal.Base.Commit)

	settle()
	got := env.Get(t, proposed.Name)
	if got.Status.Phase != v1alpha1.PhasePullRequestOpen || got.Status.VerificationStartedAt != nil {
		t.Errorf("phase = %s, started = %v, want Executing and no window while the pull request is open",
			got.Status.Phase, got.Status.VerificationStartedAt)
	}
	if requests := env.ArgoCD.Requests(); len(requests) != 0 {
		t.Errorf("Argo CD was asked before the merge: %v", requests)
	}
}

func TestTargetRecoversWhenEveryCheckPassesForTheWholeWindow(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	env.Alertmanager.Fire(alertFingerprint)
	started := verifying(t, env, tr, commit)
	// A pod that crashed before the window, and one that belongs to another Target, are not new restarts.
	env.CreateTargetPod(t, sandboxNamespace, "checkout-old", podLabels(checkout), &testenv.Termination{
		Reason: oomKilled, FinishedAt: started.Status.VerificationStartedAt.Add(-time.Minute), Last: true,
	})
	env.CreateTargetPod(t, sandboxNamespace, "other-1", podLabels("other"), &testenv.Termination{
		Reason: oomKilled, FinishedAt: started.Status.VerificationStartedAt.Add(time.Second), Last: true,
	})

	t.Run("nothing is decided before the window ends", func(t *testing.T) {
		env.Clock.Step(5*time.Minute - time.Second)
		settle()
		got := env.Get(t, tr.Name)
		if got.Status.Phase != v1alpha1.PhaseVerifying {
			t.Fatalf("phase = %s one second before the window ends, want Verifying", got.Status.Phase)
		}
		if requests := env.Alertmanager.Requests(); len(requests) != 0 {
			t.Errorf("Alertmanager was asked before the window ended: %v", requests)
		}
	})
	t.Run("the alert resolves and the window ends", func(t *testing.T) {
		env.Alertmanager.Resolve(alertFingerprint)
		env.Clock.Step(time.Second)
		got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseRecovered)
		requireChecks(t, got, map[string]bool{
			v1alpha1.CheckArgoCDSynced:     true,
			v1alpha1.CheckRolloutAvailable: true,
			v1alpha1.CheckNoNewRestarts:    true,
			v1alpha1.CheckAlertResolved:    true,
		})
	})
}

func TestVerificationRechecksArgoCDAtTheEndOfTheWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status testenv.ArgoCDStatus
	}{
		{name: "revision changed", status: testenv.ArgoCDStatus{Sync: synced, Revision: "different-commit", Health: healthy}},
		{name: "health degraded", status: testenv.ArgoCDStatus{Sync: synced, Revision: "merge-commit", Health: "Degraded"}},
		{name: "out of sync", status: testenv.ArgoCDStatus{Sync: outOfSync, Revision: "merge-commit", Health: healthy}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.Start(t, withPatchResources())
			tr, commit := merged(t, env, humanTrigger)
			verifying(t, env, tr, commit)
			c.status.Revision = strings.ReplaceAll(c.status.Revision, "merge-commit", commit)
			env.ArgoCD.Set(testenv.ArgoCDApplication, c.status)

			env.Clock.Step(5 * time.Minute)
			got := env.WaitFor(t, tr.Name, "Verification concludes", func(latest *v1alpha1.Page) bool {
				return latest.Status.Phase == v1alpha1.PhaseRecovered || latest.Status.Phase == v1alpha1.PhaseNotRecovered
			})
			if got.Status.Phase != v1alpha1.PhaseNotRecovered {
				t.Fatalf("phase = %s with final Argo CD status %+v, want NotRecovered", got.Status.Phase, c.status)
			}
			if check := checkResult(t, got, v1alpha1.CheckArgoCDSynced); check.Passed {
				t.Errorf("Argo CD check = %+v, want failed for final status %+v", check, c.status)
			}
		})
	}
}

func TestVerificationRetriesWhenFinalArgoCDCheckFails(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, humanTrigger)
	verifying(t, env, tr, commit)
	env.ArgoCD.FailNext(1_000_000)
	before := len(env.ArgoCD.Requests())
	env.Clock.Step(5 * time.Minute)
	eventually(t, "Argo CD is retried after the final check fails", func() bool {
		return len(env.ArgoCD.Requests()) >= before+3
	})
	if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhaseVerifying {
		t.Fatalf("phase = %s after Argo CD failed during the final check, want Verifying", got.Status.Phase)
	}

	env.ArgoCD.FailNext(0)
	got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseRecovered)
	check := checkResult(t, got, v1alpha1.CheckArgoCDSynced)
	if !check.Passed || !strings.Contains(check.Message, commit) {
		t.Errorf("final Argo CD check = %+v, want pass at %s", check, commit)
	}
}

func TestRestartKeepsVerificationOpenAndCannotBeForgotten(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, humanTrigger)
	started := verifying(t, env, tr, commit)
	env.Clock.Step(time.Minute)
	pod := env.CreateTargetPod(t, sandboxNamespace, "checkout-restarted", podLabels(checkout), &testenv.Termination{
		Reason: oomKilled, FinishedAt: env.Clock.Now(), Last: true,
	})
	observed := env.WaitFor(t, tr.Name, "the restart failure is recorded", func(latest *v1alpha1.Page) bool {
		for _, check := range latest.Status.VerificationChecks {
			if check.Name == v1alpha1.CheckNoNewRestarts && !check.Passed {
				return true
			}
		}
		return false
	})
	if observed.Status.Phase != v1alpha1.PhaseVerifying {
		t.Fatalf("phase = %s, want Verifying before the window ends", observed.Status.Phase)
	}
	if err := env.Client.Delete(t.Context(), pod); err != nil {
		t.Fatalf("delete restarted pod: %v", err)
	}
	if !env.Clock.Now().Before(started.Status.VerificationStartedAt.Add(5 * time.Minute)) {
		t.Fatal("test must remove the pod before the observation window ends")
	}
	env.Clock.Step(4 * time.Minute)
	got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseNotRecovered)
	check := checkResult(t, got, v1alpha1.CheckNoNewRestarts)
	if check.Passed || !strings.Contains(check.Message, oomKilled) {
		t.Errorf("restart check = %+v after its pod disappeared, want the recorded OOMKilled failure", check)
	}
}

func TestHumanTriggerSkipsTheAlertCheck(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, humanTrigger)
	// An alert that happens to fire for the Target is not this Page's trigger.
	env.Alertmanager.Fire(alertFingerprint)
	verifying(t, env, tr, commit)

	env.Clock.Step(5 * time.Minute)
	got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseRecovered)
	requireChecks(t, got, map[string]bool{
		v1alpha1.CheckArgoCDSynced:     true,
		v1alpha1.CheckRolloutAvailable: true,
		v1alpha1.CheckNoNewRestarts:    true,
		v1alpha1.CheckAlertResolved:    true,
	})
	if message := checkResult(t, got, v1alpha1.CheckAlertResolved).Message; !strings.Contains(message, "skipped") {
		t.Errorf("alert check message = %q, want it to say the check was skipped", message)
	}
	if requests := env.Alertmanager.Requests(); len(requests) != 0 {
		t.Errorf("Alertmanager was asked for a human Trigger: %v", requests)
	}
}

func TestTargetIsNotRecovered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		trigger v1alpha1.Trigger
		// arrange runs once Verification has begun and before the window ends.
		arrange func(t *testing.T, env *testenv.Env, tr *v1alpha1.Page)
		failed  map[string]bool
		message string
	}{
		{
			name:    "the triggering alert is still firing",
			trigger: alertTrigger,
			arrange: func(t *testing.T, env *testenv.Env, _ *v1alpha1.Page) {
				env.Alertmanager.Fire(alertFingerprint)
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: false,
			},
			message: "still firing",
		},
		{
			name:    "the triggering alert is silenced, which does not resolve it",
			trigger: alertTrigger,
			arrange: func(t *testing.T, env *testenv.Env, _ *v1alpha1.Page) {
				env.Alertmanager.Silence(alertFingerprint)
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: false,
			},
			message: "still firing",
		},
		{
			name:    "an alert Trigger carries no fingerprint to confirm",
			trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceAlertmanager},
			arrange: func(*testing.T, *testenv.Env, *v1alpha1.Page) {},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: false,
			},
			message: "no alert fingerprint",
		},
		{
			name:    "the rollout is not available",
			trigger: humanTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Page) {
				env.SetDeploymentReplicas(t, sandboxNamespace, tr.Spec.Target.Name, 1, 0)
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: false,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: true,
			},
			message: "0 of 1 replicas are available",
		},
		{
			name:    "the Target is gone",
			trigger: humanTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Page) {
				d := liveDeployment(t, env, tr.Spec.Target.Name)
				if err := env.Client.Delete(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: false,
				v1alpha1.CheckNoNewRestarts: false, v1alpha1.CheckAlertResolved: true,
			},
			message: "no longer exists",
		},
		{
			name:    "a container is OOMKilled during the window",
			trigger: alertTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Page) {
				env.Alertmanager.Fire(alertFingerprint)
				env.Clock.Step(time.Minute)
				env.CreateTargetPod(t, sandboxNamespace, "checkout-new", podLabels(tr.Spec.Target.Name),
					&testenv.Termination{Reason: oomKilled, FinishedAt: env.Clock.Now(), Last: true})
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: false, v1alpha1.CheckAlertResolved: false,
			},
			message: oomKilled,
		},
		{
			name:    "a container exits during the window",
			trigger: humanTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Page) {
				env.Clock.Step(time.Minute)
				env.CreateTargetPod(t, sandboxNamespace, "checkout-new", podLabels(tr.Spec.Target.Name),
					&testenv.Termination{Reason: "Error", FinishedAt: env.Clock.Now()})
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: false, v1alpha1.CheckAlertResolved: true,
			},
			message: "container app of pod checkout-new ended Error",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.Start(t, withPatchResources())
			tr, commit := merged(t, env, c.trigger)
			started := verifying(t, env, tr, commit)
			c.arrange(t, env, tr)
			settle()
			if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhaseVerifying {
				t.Fatalf("phase = %s before the window ended, want Verifying", got.Status.Phase)
			}
			remaining := started.Status.VerificationStartedAt.Add(5 * time.Minute).Sub(env.Clock.Now())
			if remaining > 0 {
				env.Clock.Step(remaining)
			}
			got := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseNotRecovered)
			requireChecks(t, got, c.failed)
			var failedMessages []string
			for _, check := range got.Status.VerificationChecks {
				if !check.Passed {
					failedMessages = append(failedMessages, check.Message)
				}
			}
			if !strings.Contains(strings.Join(failedMessages, "\n"), c.message) {
				t.Errorf("failed checks say %q, want one to mention %q", failedMessages, c.message)
			}
		})
	}
}

func TestVerificationNeverGuessesWhenAlertmanagerIsUnreachable(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	verifying(t, env, tr, commit)
	env.Alertmanager.FailNext(1_000_000)

	env.Clock.Step(5 * time.Minute)
	eventually(t, "Alertmanager is asked repeatedly", func() bool { return len(env.Alertmanager.Requests()) >= 3 })
	got := env.Get(t, tr.Name)
	if got.Status.Phase != v1alpha1.PhaseVerifying || len(got.Status.VerificationChecks) != 1 {
		t.Fatalf("phase = %s with checks %+v while Alertmanager fails, want Verifying with only the sync check",
			got.Status.Phase, got.Status.VerificationChecks)
	}

	env.Alertmanager.FailNext(0)
	env.WaitForPhase(t, tr.Name, v1alpha1.PhaseRecovered)
}

func TestAlertWithAnEndTimeInThePastIsResolved(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	verifying(t, env, tr, commit)
	env.Alertmanager.EndAt(alertFingerprint, env.Clock.Now().Add(2*time.Minute))

	// Alertmanager still lists the alert, but it ended before the window did.
	env.Clock.Step(5 * time.Minute)
	env.WaitForPhase(t, tr.Name, v1alpha1.PhaseRecovered)
}

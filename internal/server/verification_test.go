package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// podLabels are the labels that the Workload's Deployment selects its pods by.
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

// merged returns a Triage for the checkout Workload, started by the Trigger, whose approved pull request has
// merged, with the merge commit that Argo CD has not yet synced.
func merged(t *testing.T, env *testenv.Env, trigger v1alpha1.Trigger) (*v1alpha1.Triage, string) {
	t.Helper()
	deploy(t, env, workload{name: checkout})
	candidate := testenv.NewTriage(checkout)
	candidate.Spec.Trigger = trigger
	created := env.CreateTriage(t, candidate)
	agent := env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), patchReport("app", nil, quantities("memory", "512Mi")))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	commit := env.GitHub.CompleteChecks(t, 1)
	tr := env.WaitFor(t, proposed.Name, "the merge commit is recorded", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Execution != nil && tr.Status.Execution.MergeCommit != ""
	})
	return tr, commit
}

// verifying returns the Triage once Argo CD has synced the merge commit and Verification has begun. The
// Deployment's rollout completes first, as it does while Argo CD's sync converges.
func verifying(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage, commit string) *v1alpha1.Triage {
	t.Helper()
	env.MarkDeploymentAvailable(t, sandboxNamespace, tr.Spec.Workload.Name)
	env.ArgoCD.Sync(commit)
	return env.WaitForPhase(t, tr.Name, v1alpha1.PhaseVerifying)
}

// checkResult returns the recorded result of the check, and fails the test if there is none.
func checkResult(t *testing.T, tr *v1alpha1.Triage, name string) v1alpha1.VerificationCheck {
	t.Helper()
	for _, c := range tr.Status.VerificationChecks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("Triage %s records no result for check %s: %+v", tr.Name, name, tr.Status.VerificationChecks)
	return v1alpha1.VerificationCheck{}
}

// requireChecks checks which of the checks passed.
func requireChecks(t *testing.T, tr *v1alpha1.Triage, want map[string]bool) {
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

// triagesFollowing lists the Triages that follow the named Triage.
func triagesFollowing(t *testing.T, env *testenv.Env, name string) []v1alpha1.Triage {
	t.Helper()
	var all v1alpha1.TriageList
	if err := env.Client.List(t.Context(), &all, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	var following []v1alpha1.Triage
	for _, tr := range all.Items {
		if tr.Spec.FollowsTriage == name {
			following = append(following, tr)
		}
	}
	return following
}

// requireFollowUp checks that the NotRecovered Triage started exactly one Follow-up Triage, which references it,
// and that the original records the reference.
func requireFollowUp(t *testing.T, env *testenv.Env, original *v1alpha1.Triage) *v1alpha1.Triage {
	t.Helper()
	recorded := env.WaitFor(t, original.Name, "the Follow-up Triage is recorded", func(tr *v1alpha1.Triage) bool {
		return tr.Status.FollowUpTriage != ""
	})
	following := triagesFollowing(t, env, original.Name)
	if len(following) != 1 || following[0].Name != recorded.Status.FollowUpTriage {
		t.Fatalf("Triages following %s = %d, recorded reference %q, want exactly the recorded one",
			original.Name, len(following), recorded.Status.FollowUpTriage)
	}
	followUp := &following[0]
	if followUp.Spec.Workload != original.Spec.Workload {
		t.Errorf("Follow-up Triage targets %+v, want %+v", followUp.Spec.Workload, original.Spec.Workload)
	}
	trigger := followUp.Spec.Trigger
	if trigger.Source != v1alpha1.TriggerSourceFollowUp ||
		trigger.AlertFingerprint != original.Spec.Trigger.AlertFingerprint {
		t.Errorf("Follow-up Triage trigger = %+v, want source FollowUp and the original's alert fingerprint", trigger)
	}
	// The Follow-up Triage is an ordinary Triage: the controller starts its investigation.
	env.WaitFor(t, followUp.Name, "the Follow-up Triage is investigated", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseInvestigating
	})
	return followUp
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
			if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhaseExecuting {
				t.Fatalf("Argo CD reports %+v, but the phase is %s, want Executing", status, got.Status.Phase)
			}
		}
	})
	t.Run("Argo CD errors do not start it either", func(t *testing.T) {
		env.ArgoCD.FailNext(1_000_000)
		env.ArgoCD.Sync(commit)
		before := len(env.ArgoCD.Requests())
		eventually(t, "Argo CD is asked again", func() bool { return len(env.ArgoCD.Requests()) >= before+3 })
		if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhaseExecuting {
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
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	// Argo CD is healthy at the commit the pull request was based on, as it is before any change.
	env.MarkDeploymentAvailable(t, sandboxNamespace, checkout)
	env.ArgoCD.Sync(proposed.Status.Proposal.Base.Commit)

	settle()
	got := env.Get(t, proposed.Name)
	if got.Status.Phase != v1alpha1.PhaseExecuting || got.Status.VerificationStartedAt != nil {
		t.Errorf("phase = %s, started = %v, want Executing and no window while the pull request is open",
			got.Status.Phase, got.Status.VerificationStartedAt)
	}
	if requests := env.ArgoCD.Requests(); len(requests) != 0 {
		t.Errorf("Argo CD was asked before the merge: %v", requests)
	}
}

func TestWorkloadRecoversWhenEveryCheckPassesForTheWholeWindow(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	env.Alertmanager.Fire(alertFingerprint)
	started := verifying(t, env, tr, commit)
	// A pod that crashed before the window, and one that belongs to another Workload, are not new restarts.
	env.CreateWorkloadPod(t, sandboxNamespace, "checkout-old", podLabels(checkout), &testenv.Termination{
		Reason: oomKilled, FinishedAt: started.Status.VerificationStartedAt.Add(-time.Minute), Last: true,
	})
	env.CreateWorkloadPod(t, sandboxNamespace, "other-1", podLabels("other"), &testenv.Termination{
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
		if got.Status.FollowUpTriage != "" || len(triagesFollowing(t, env, tr.Name)) != 0 {
			t.Errorf("a Recovered Triage started a Follow-up Triage: %q", got.Status.FollowUpTriage)
		}
	})
}

func TestHumanTriggerSkipsTheAlertCheck(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, humanTrigger)
	// An alert that happens to fire for the Workload is not this Triage's trigger.
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

func TestWorkloadIsNotRecovered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		trigger v1alpha1.Trigger
		// arrange runs once Verification has begun and before the window ends. It returns whether the Triage is
		// decided without waiting for the window to end.
		arrange func(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) (immediate bool)
		failed  map[string]bool
		message string
	}{
		{
			name:    "the triggering alert is still firing",
			trigger: alertTrigger,
			arrange: func(t *testing.T, env *testenv.Env, _ *v1alpha1.Triage) bool {
				env.Alertmanager.Fire(alertFingerprint)
				return false
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
			arrange: func(t *testing.T, env *testenv.Env, _ *v1alpha1.Triage) bool {
				env.Alertmanager.Silence(alertFingerprint)
				return false
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
			arrange: func(*testing.T, *testenv.Env, *v1alpha1.Triage) bool { return false },
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: true,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: false,
			},
			message: "no alert fingerprint",
		},
		{
			name:    "the rollout is not available",
			trigger: humanTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) bool {
				env.SetDeploymentReplicas(t, sandboxNamespace, tr.Spec.Workload.Name, 1, 0)
				return false
			},
			failed: map[string]bool{
				v1alpha1.CheckArgoCDSynced: true, v1alpha1.CheckRolloutAvailable: false,
				v1alpha1.CheckNoNewRestarts: true, v1alpha1.CheckAlertResolved: true,
			},
			message: "0 of 1 replicas are available",
		},
		{
			name:    "the Workload is gone",
			trigger: humanTrigger,
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) bool {
				d := liveDeployment(t, env, tr.Spec.Workload.Name)
				if err := env.Client.Delete(t.Context(), d); err != nil {
					t.Fatal(err)
				}
				return true
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
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) bool {
				env.Alertmanager.Fire(alertFingerprint)
				env.Clock.Step(time.Minute)
				env.CreateWorkloadPod(t, sandboxNamespace, "checkout-new", podLabels(tr.Spec.Workload.Name),
					&testenv.Termination{Reason: oomKilled, FinishedAt: env.Clock.Now(), Last: true})
				return true
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
			arrange: func(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) bool {
				env.Clock.Step(time.Minute)
				env.CreateWorkloadPod(t, sandboxNamespace, "checkout-new", podLabels(tr.Spec.Workload.Name),
					&testenv.Termination{Reason: "Error", FinishedAt: env.Clock.Now()})
				return true
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
			verifying(t, env, tr, commit)
			immediate := c.arrange(t, env, tr)

			if !immediate {
				settle()
				if got := env.Get(t, tr.Name); got.Status.Phase != v1alpha1.PhaseVerifying {
					t.Fatalf("phase = %s before the window ended, want Verifying", got.Status.Phase)
				}
				env.Clock.Step(5 * time.Minute)
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
			requireFollowUp(t, env, got)
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

func TestFollowUpTriageIsStartedOnce(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	env.Alertmanager.Fire(alertFingerprint)
	verifying(t, env, tr, commit)
	env.Clock.Step(5 * time.Minute)
	notRecovered := env.WaitForPhase(t, tr.Name, v1alpha1.PhaseNotRecovered)
	followUp := requireFollowUp(t, env, notRecovered)

	t.Run("the original is not decided again", func(t *testing.T) {
		env.Clock.Step(time.Hour)
		settle()
		got := env.Get(t, tr.Name)
		if got.Status.Phase != v1alpha1.PhaseNotRecovered || got.Status.FollowUpTriage != followUp.Name ||
			len(triagesFollowing(t, env, tr.Name)) != 1 {
			t.Errorf("phase = %s, follow-up = %q, want NotRecovered, %q, and one Follow-up Triage",
				got.Status.Phase, got.Status.FollowUpTriage, followUp.Name)
		}
	})
	t.Run("a lost reference is restored without starting another", func(t *testing.T) {
		writeStatus(t, serverClient(t, env), tr.Name, func(latest *v1alpha1.Triage) { latest.Status.FollowUpTriage = "" })
		got := env.WaitFor(t, tr.Name, "the reference is restored", func(tr *v1alpha1.Triage) bool {
			return tr.Status.FollowUpTriage != ""
		})
		if got.Status.FollowUpTriage != followUp.Name || len(triagesFollowing(t, env, tr.Name)) != 1 {
			t.Errorf("follow-up = %q among %d, want %q alone", got.Status.FollowUpTriage,
				len(triagesFollowing(t, env, tr.Name)), followUp.Name)
		}
	})
}

func TestFollowUpTriageYieldsToAnActiveTriage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	asServer := serverClient(t, env)
	original := env.CreateTriage(t, testenv.NewTriage(checkout))
	env.WaitForPhase(t, original.Name, v1alpha1.PhaseInvestigating)
	// The Workload's Triage ends some other way, and a new Trigger starts the Workload's next Triage.
	writeStatus(t, asServer, original.Name, func(latest *v1alpha1.Triage) { latest.Status.Phase = v1alpha1.PhaseFailed })
	active := env.CreateTriage(t, testenv.NewTriage(checkout))

	// The first Triage is then ended NotRecovered, which refuses to start a Follow-up Triage beside the active one.
	writeStatus(t, asServer, original.Name, func(latest *v1alpha1.Triage) {
		latest.Status.Phase = v1alpha1.PhaseNotRecovered
	})
	got := env.WaitFor(t, original.Name, "the Follow-up Triage is skipped", func(tr *v1alpha1.Triage) bool {
		return meta.IsStatusConditionTrue(tr.Status.Conditions, v1alpha1.ConditionFollowUpSkipped)
	})
	condition := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionFollowUpSkipped)
	if condition.Reason != v1alpha1.ReasonActiveTriageExists || !strings.Contains(condition.Message, active.Name) ||
		got.Status.FollowUpTriage != "" {
		t.Errorf("condition = %+v, follow-up = %q, want reason %s naming %s and no reference",
			condition, got.Status.FollowUpTriage, v1alpha1.ReasonActiveTriageExists, active.Name)
	}
	settle()
	if following := triagesFollowing(t, env, original.Name); len(following) != 0 {
		t.Errorf("Triages following %s = %d, want none", original.Name, len(following))
	}
}

func TestOnlyTheServerMovesATriageToRecovered(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	verifying(t, env, tr, commit)

	// An administrator cannot declare the outcome either: Verification belongs to deterministic server code.
	got := env.Get(t, tr.Name)
	got.Status.Phase = v1alpha1.PhaseRecovered
	got.Status.VerificationChecks = append(got.Status.VerificationChecks, v1alpha1.VerificationCheck{
		Name: v1alpha1.CheckAlertResolved, Passed: true,
	})
	if err := env.Client.Status().Update(t.Context(), got); err == nil {
		t.Fatal("an administrator wrote the Verification outcome")
	}
	if now := env.Get(t, tr.Name); now.Status.Phase != v1alpha1.PhaseVerifying {
		t.Errorf("phase = %s, want Verifying", now.Status.Phase)
	}
}

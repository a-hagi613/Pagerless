package server_test

import (
	"net/http"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/controller"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// bypassTriageCreate simulates the admission race only in this isolated API server. Production manifests are
// unchanged, and status/Approval webhooks remain installed. The returned closure restores the create webhook.
func bypassTriageCreate(t *testing.T, env *testenv.Env) func() {
	t.Helper()
	var config admissionregistrationv1.ValidatingWebhookConfiguration
	key := client.ObjectKey{Name: server.DefaultServerServiceAccount}
	if err := env.Client.Get(t.Context(), key, &config); err != nil {
		t.Fatal(err)
	}
	before := config.DeepCopy()
	config.Webhooks = nil
	for _, webhook := range before.Webhooks {
		if webhook.Name != "create.triages.pagerless.dev" {
			config.Webhooks = append(config.Webhooks, webhook)
		}
	}
	if err := env.Client.Update(t.Context(), &config); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := env.Client.Get(t.Context(), key, &config); err != nil {
			t.Fatal(err)
		}
		config.Webhooks = before.Webhooks
		if err := env.Client.Update(t.Context(), &config); err != nil {
			t.Fatal(err)
		}
	}
}

func namedTriage(name string) *v1alpha1.Triage {
	tr := testenv.NewTriage(checkout)
	tr.Name, tr.GenerateName = name, ""
	return tr
}

func TestDuplicateTriageFallbackKeepsOneAgentAndFailsTheYoungerTriage(t *testing.T) {
	t.Parallel()
	var restore func()
	env := testenv.Start(t, testenv.WithValidators(func(env *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		restore = bypassTriageCreate(t, env)
		for _, name := range []string{"duplicate-a", "duplicate-z"} {
			tr := namedTriage(name)
			if err := env.Client.Create(t.Context(), tr); err != nil {
				t.Fatal(err)
			}
			if name == "duplicate-z" {
				// A crash can leave a Job before its name was recorded in Triage status.
				job := &batchv1.Job{
					Name: controller.AgentJobName(name), Namespace: testenv.Namespace,
					OwnerReferences: []metav1.OwnerReference{
						*metav1.NewControllerRef(tr, v1alpha1.GroupVersion.WithKind("Triage")),
					},
					Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "agent", Image: testenv.AgentImage}},
					}}},
				}
				if err := env.Client.Create(t.Context(), job); err != nil {
					t.Fatal(err)
				}
			}
		}
		return proposal.DefaultValidators()
	}))
	restore()
	winner := env.WaitForPhase(t, "duplicate-a", v1alpha1.PhaseInvestigating)
	loser := env.WaitFor(t, "duplicate-z", "the duplicate is reconciled", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseFailed || tr.Status.Phase == v1alpha1.PhaseInvestigating
	})
	condition := meta.FindStatusCondition(loser.Status.Conditions, v1alpha1.ConditionFailed)
	if loser.Status.Phase != v1alpha1.PhaseFailed || condition == nil || condition.Reason != "Duplicate" {
		t.Fatalf("duplicate phase = %s, condition = %+v, want Failed with Duplicate", loser.Status.Phase, condition)
	}
	var jobs batchv1.JobList
	if err := env.Client.List(t.Context(), &jobs, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || !metav1.IsControlledBy(&jobs.Items[0], winner) {
		t.Fatalf("Jobs = %+v, want only the canonical Triage's agent", jobs.Items)
	}
	if loser.Status.Execution != nil {
		t.Fatal("the duplicate Triage executed a Proposal")
	}
	requireNothingDelivered(t, env)
	agent := env.StartAgent(t, winner.Name)
	result, err := agent.API.PostReport(t.Context(), noActionReport())
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("canonical agent report = %+v, %v", result, err)
	}
	env.WaitForPhase(t, winner.Name, v1alpha1.PhaseProposed)
}

func TestLateDuplicateDoesNotPreemptAnExecutingTriageOrItsPullRequest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	approve(t, env, proposed)
	executing := withPullRequest(t, env, proposed.Name)
	restore := bypassTriageCreate(t, env)
	late := namedTriage("a-late-duplicate")
	if err := env.Client.Create(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	restore()
	env.WaitForPhase(t, late.Name, v1alpha1.PhaseFailed)
	winner := env.Get(t, executing.Name)
	if winner.Status.Phase != v1alpha1.PhaseExecuting || winner.Status.Execution == nil ||
		winner.Status.Execution.PullRequestURL != executing.Status.Execution.PullRequestURL {
		t.Fatalf("existing execution changed after a late duplicate: %+v", winner.Status)
	}
	if prs := env.GitHub.PullRequests(); len(prs) != 1 || prs[0].State != testenv.PROpen {
		t.Fatalf("pull requests = %+v, want the existing pull request left open", prs)
	}
}

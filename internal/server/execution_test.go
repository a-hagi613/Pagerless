package server_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/publish"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// approve has Alice approve the Triage's current Proposal.
func approve(t *testing.T, env *testenv.Env, tr *v1alpha1.Triage) {
	t.Helper()
	alice := approverClient(t, env, aliceUser)
	if err := alice.Create(t.Context(), decision(tr, v1alpha1.DecisionApproved, "memory limit looks right")); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// withPullRequest returns the Triage once pagerless-server has published its pull request and recorded it.
func withPullRequest(t *testing.T, env *testenv.Env, name string) *v1alpha1.Triage {
	t.Helper()
	return env.WaitFor(t, name, "the pull request is recorded", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseExecuting && tr.Status.Execution != nil &&
			tr.Status.Execution.PullRequestURL != ""
	})
}

// serverClient returns a client with pagerless-server's identity, the only one that may write Triage status.
func serverClient(t *testing.T, env *testenv.Env) client.Client {
	t.Helper()
	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(env.ServerConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create a client with the server's identity: %v", err)
	}
	return c
}

// writeStatus changes a Triage's status as the client's identity, retrying when the server wrote it first.
func writeStatus(t *testing.T, c client.Client, name string, mutate func(*v1alpha1.Triage)) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var tr v1alpha1.Triage
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: testenv.Namespace, Name: name}, &tr); err != nil {
			return err
		}
		mutate(&tr)
		return c.Status().Update(t.Context(), &tr)
	})
	if err != nil {
		t.Fatalf("write the status of Triage %s: %v", name, err)
	}
}

// requireNothingDelivered checks that nothing reached the Sandbox or GitHub: no branch, no pull request.
func requireNothingDelivered(t *testing.T, env *testenv.Env) {
	t.Helper()
	if prs := env.GitHub.PullRequests(); len(prs) != 0 {
		t.Errorf("pull requests = %+v, want none", prs)
	}
	if branches := env.Sandbox.Branches(); !slices.Equal(branches, []string{"main"}) {
		t.Errorf("Sandbox branches = %v, want only main", branches)
	}
}

// requireFailed waits for the Triage to end as Failed and checks the condition's reason and message.
func requireFailed(t *testing.T, env *testenv.Env, name, reason, message string) *v1alpha1.Triage {
	t.Helper()
	tr := env.WaitForPhase(t, name, v1alpha1.PhaseFailed)
	condition := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionFailed)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != reason ||
		!strings.Contains(condition.Message, message) {
		t.Errorf("Failed condition = %+v, want reason %s mentioning %q", condition, reason, message)
	}
	return tr
}

func TestApprovedProposalBecomesAnAutoMergingPullRequest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	w := workload{name: checkout}
	deploy(t, env, w)
	proposed := proposedTriage(t, env, checkout)
	hash, base := proposed.Status.Proposal.Hash, proposed.Status.Proposal.Base.Commit
	patch := proposed.Status.Proposal.RenderedChange.Patch

	approve(t, env, proposed)
	tr := withPullRequest(t, env, proposed.Name)

	prs := env.GitHub.PullRequests()
	if len(prs) != 1 {
		t.Fatalf("pull requests = %+v, want exactly one", prs)
	}
	pr := prs[0]
	if pr.URL != tr.Status.Execution.PullRequestURL {
		t.Errorf("recorded pull request = %q, want %q", tr.Status.Execution.PullRequestURL, pr.URL)
	}
	if !pr.AutoMerge || pr.AutoMergeMethod != "SQUASH" || pr.State != testenv.PROpen || pr.Base != "main" {
		t.Errorf("pull request = %+v, want open into main with squash auto-merge enabled", pr)
	}
	if want, _ := publish.Branch(proposed); pr.Head != want {
		t.Errorf("head branch = %q, want %q", pr.Head, want)
	}
	for _, want := range []string{proposed.Name, hash, base, aliceUser, "memory limit looks right"} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("pull request body lacks %q:\n%s", want, pr.Body)
		}
	}
	if tr.Status.Execution.ExecutedAt == nil || tr.Status.Execution.MergeCommit != "" {
		t.Errorf("execution = %+v, want a time and no merge commit yet", tr.Status.Execution)
	}

	t.Run("the branch is the stored patch, byte for byte, on its base commit", func(t *testing.T) {
		branch := "refs/heads/" + pr.Head
		if parent := env.Sandbox.Rev(branch + "^"); parent != base {
			t.Errorf("parent of the commit = %s, want the base %s", parent, base)
		}
		if got := env.Sandbox.Diff(base, branch); got != patch {
			t.Errorf("diff of the commit differs from the stored patch:\n%s\nwant:\n%s", got, patch)
		}
		if message := env.Sandbox.Message(branch); !strings.Contains(message, "Pagerless-Proposal-Hash: "+hash) {
			t.Errorf("commit message lacks the hash:\n%s", message)
		}
	})
	t.Run("nothing merges before the check passes", func(t *testing.T) {
		if head := env.Sandbox.Head(); head != base {
			t.Errorf("Sandbox main = %s, want it unchanged at %s", head, base)
		}
	})
	t.Run("the merge commit is recorded once the pull request merges", func(t *testing.T) {
		merged := env.GitHub.CompleteChecks(t, pr.Number)
		got := env.WaitFor(t, proposed.Name, "the merge commit is recorded", func(tr *v1alpha1.Triage) bool {
			return tr.Status.Execution != nil && tr.Status.Execution.MergeCommit != ""
		})
		if got.Status.Execution.MergeCommit != merged || got.Status.Execution.PullRequestURL != pr.URL {
			t.Errorf("execution = %+v, want merge commit %s on %s", got.Status.Execution, merged, pr.URL)
		}
		if got.Status.Phase != v1alpha1.PhaseExecuting {
			t.Errorf("phase = %s, want Executing until Verification takes over", got.Status.Phase)
		}
		if !strings.Contains(env.Sandbox.File("main", w.sourcePath()), "memory: 512Mi") {
			t.Errorf("Sandbox main does not hold the approved limit after the merge")
		}
	})
}

func TestExecutionIsIdempotentPerProposal(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	branch := "refs/heads/" + env.GitHub.PullRequests()[0].Head
	before := env.Sandbox.Rev(branch)

	// Touch the Triage so that the execution reconciler runs again for the same Proposal.
	key := client.ObjectKey{Namespace: testenv.Namespace, Name: proposed.Name}
	var tr v1alpha1.Triage
	if err := env.Client.Get(t.Context(), key, &tr); err != nil {
		t.Fatal(err)
	}
	tr.Labels = map[string]string{"pagerless.dev/touched": "true"}
	if err := env.Client.Update(t.Context(), &tr); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Get(t.Context(), key, &tr); err == nil {
		tr.Labels["pagerless.dev/touched"] = "again"
		_ = env.Client.Update(t.Context(), &tr)
	}

	if prs := env.GitHub.PullRequests(); len(prs) != 1 {
		t.Errorf("pull requests = %+v, want still one", prs)
	}
	if got := env.Sandbox.Rev(branch); got != before {
		t.Errorf("the branch moved from %s to %s", before, got)
	}
}

func TestExecutionRecoversWhenAutoMergeFails(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	// The pull request opens, but turning on auto-merge fails twice.
	env.GitHub.FailNext(http.MethodPost, "/graphql", http.StatusBadGateway, 2)

	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	env.WaitFor(t, proposed.Name, "auto-merge is enabled on the pull request", func(*v1alpha1.Triage) bool {
		prs := env.GitHub.PullRequests()
		return len(prs) == 1 && prs[0].AutoMerge
	})
	if prs := env.GitHub.PullRequests(); len(prs) != 1 {
		t.Errorf("pull requests = %+v, want exactly one after the retries", prs)
	}
	if branches := env.Sandbox.Branches(); len(branches) != 2 {
		t.Errorf("Sandbox branches = %v, want main and one pull request branch", branches)
	}
}

func TestClosedPullRequestEndsTheTriageAsFailed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	approve(t, env, proposed)
	tr := withPullRequest(t, env, proposed.Name)

	env.GitHub.Close(t, 1)
	failed := requireFailed(t, env, proposed.Name, v1alpha1.ReasonPullRequestClosed, tr.Status.Execution.PullRequestURL)
	if failed.Status.Execution == nil || failed.Status.Execution.MergeCommit != "" {
		t.Errorf("execution = %+v, want the pull request kept and no merge commit", failed.Status.Execution)
	}
}

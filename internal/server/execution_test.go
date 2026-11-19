package server_test

import (
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/publish"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// withPullRequest returns the Page once pagerless-server has published its pull
// request and recorded it.
func withPullRequest(t *testing.T, env *testenv.Env, name string) *v1alpha1.Page {
	t.Helper()
	return env.WaitFor(t, name, "the pull request is recorded", func(p *v1alpha1.Page) bool {
		return p.Status.Phase == v1alpha1.PhasePullRequestOpen && p.Status.PullRequest != nil &&
			p.Status.PullRequest.URL != ""
	})
}

func TestAnApprovedProposalBecomesAPullRequest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	w := target{name: checkout}
	deploy(t, env, w)
	proposed := proposedPage(t, env, checkout)
	hash, base := proposed.Status.Proposal.Hash, proposed.Status.Proposal.Base.Commit
	patch := proposed.Status.Proposal.RenderedChange.Patch

	approve(t, env, proposed)
	page := withPullRequest(t, env, proposed.Name)

	pullRequests := env.GitHub.PullRequests()
	if len(pullRequests) != 1 {
		t.Fatalf("pull requests = %+v, want exactly one", pullRequests)
	}
	pullRequest := pullRequests[0]
	recorded := page.Status.PullRequest
	if recorded.URL != pullRequest.URL || recorded.Number != 1 || recorded.Provider != "GitHub" ||
		recorded.State != v1alpha1.PullRequestOpen {
		t.Errorf("recorded pull request = %+v, want the open pull request %d at %s",
			recorded, pullRequest.Number, pullRequest.URL)
	}
	if want, _ := publish.Branch(proposed); pullRequest.Head != want || recorded.Branch != want {
		t.Errorf("head branch = %q, want %q", pullRequest.Head, want)
	}
	for _, want := range []string{proposed.Name, hash, base, aliceUser} {
		if !strings.Contains(pullRequest.Body, want) {
			t.Errorf("pull request body lacks %q:\n%s", want, pullRequest.Body)
		}
	}
	if page.Status.PullRequest.OpenedAt == nil || page.Status.PullRequest.MergeCommit != "" {
		t.Errorf("pull request = %+v, want a time and no merge commit yet", page.Status.PullRequest)
	}
	condition := metaCondition(page, v1alpha1.ConditionDelivered)
	if condition == nil || condition.Reason != v1alpha1.ReasonPullRequestOpen {
		t.Errorf("Delivered condition = %+v, want True/PullRequestOpen", condition)
	}

	branch := "refs/heads/" + pullRequest.Head
	if parent := env.Sandbox.Rev(branch + "^"); parent != base {
		t.Errorf("parent of the commit = %s, want the base %s", parent, base)
	}
	if got := env.Sandbox.Diff(base, branch); got != patch {
		t.Errorf("diff of the commit differs from the stored patch:\n%s\nwant:\n%s", got, patch)
	}
	if message := env.Sandbox.Message(branch); !strings.Contains(message, "Pagerless-Proposal-Hash: "+hash) {
		t.Errorf("commit message lacks the hash:\n%s", message)
	}
	if head := env.Sandbox.Head(); head != base {
		t.Errorf("the target branch = %s, want it unchanged at %s before the merge", head, base)
	}

	merged := env.GitHub.CompleteChecks(t, pullRequest.Number)
	got := env.WaitFor(t, proposed.Name, "the merge commit is recorded", func(p *v1alpha1.Page) bool {
		return p.Status.PullRequest != nil && p.Status.PullRequest.MergeCommit != ""
	})
	if got.Status.PullRequest.MergeCommit != merged || got.Status.PullRequest.State != v1alpha1.PullRequestMerged {
		t.Errorf("pull request = %+v, want merge commit %s", got.Status.PullRequest, merged)
	}
	if condition := metaCondition(got, v1alpha1.ConditionDelivered); condition == nil ||
		condition.Reason != v1alpha1.ReasonMerged {
		t.Errorf("Delivered condition = %+v, want True/Merged", condition)
	}
	if !strings.Contains(env.Sandbox.File("main", w.sourcePath()), "memory: 512Mi") {
		t.Errorf("the target branch does not hold the approved limit after the merge")
	}
}

func TestPublicationIsIdempotentPerProposal(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, target{name: checkout})
	proposed := proposedPage(t, env, checkout)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	branch := "refs/heads/" + env.GitHub.PullRequests()[0].Head
	before := env.Sandbox.Rev(branch)

	// Touch the Page so that the pull request reconciler runs again for the same Proposal.
	key := client.ObjectKey{Namespace: testenv.Namespace, Name: proposed.Name}
	var page v1alpha1.Page
	if err := env.Client.Get(t.Context(), key, &page); err != nil {
		t.Fatal(err)
	}
	page.Labels = map[string]string{"pagerless.dev/touched": labelTrue}
	if err := env.Client.Update(t.Context(), &page); err != nil {
		t.Fatal(err)
	}

	if pullRequests := env.GitHub.PullRequests(); len(pullRequests) != 1 {
		t.Errorf("pull requests = %+v, want still one", pullRequests)
	}
	if got := env.Sandbox.Rev(branch); got != before {
		t.Errorf("the branch moved from %s to %s", before, got)
	}
}

func TestAClosedPullRequestEndsThePageAsFailed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, target{name: checkout})
	proposed := proposedPage(t, env, checkout)
	approve(t, env, proposed)
	page := withPullRequest(t, env, proposed.Name)

	env.GitHub.Close(t, 1)
	failed := env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseFailed)
	condition := metaCondition(failed, v1alpha1.ConditionFailed)
	if condition == nil || condition.Reason != v1alpha1.ReasonPullRequestClosed {
		t.Fatalf("Failed condition = %+v, want PullRequestClosed", condition)
	}
	if !strings.Contains(condition.Message, page.Status.PullRequest.URL) {
		t.Errorf("failure message = %q, want it to name the pull request", condition.Message)
	}
	if failed.Status.PullRequest == nil || failed.Status.PullRequest.MergeCommit != "" {
		t.Errorf("pull request = %+v, want it kept with no merge commit", failed.Status.PullRequest)
	}
}

// requireNothingDelivered asserts that no pull request was opened.
func requireNothingDelivered(t *testing.T, env *testenv.Env) {
	t.Helper()
	if pullRequests := env.GitHub.PullRequests(); len(pullRequests) != 0 {
		t.Fatalf("pull requests = %+v, want none", pullRequests)
	}
}

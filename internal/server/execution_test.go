package server_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
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

// withPullRequest returns the Triage once the Executor has opened its pull request and the server recorded it.
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

// requireRefused checks that the Executor refused the request with the status and reason.
func requireRefused(t *testing.T, err error, status int, reason executorapi.Reason) {
	t.Helper()
	var refusal *executorapi.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v, want a refusal from the Executor", err)
	}
	if refusal.StatusCode != status || refusal.Reason != reason {
		t.Errorf("refusal = %d %s (%s), want %d %s", refusal.StatusCode, refusal.Reason, refusal.Message, status, reason)
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
	if want := "pagerless/" + proposed.Name + "-" + hash[:12]; pr.Head != want {
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

func TestExecutionIsIdempotentPerProposalHash(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	hash := proposed.Status.Proposal.Hash
	approve(t, env, proposed)
	tr := withPullRequest(t, env, proposed.Name)
	branchBefore := env.Sandbox.Rev("refs/heads/" + env.GitHub.PullRequests()[0].Head)
	executor := env.ServerExecutorClient(t)

	t.Run("a repeated request returns the existing pull request", func(t *testing.T) {
		for range 3 {
			response, err := executor.Execute(t.Context(), proposed.Name, hash)
			if err != nil {
				t.Fatalf("repeat: %v", err)
			}
			if response.PullRequestURL != tr.Status.Execution.PullRequestURL || response.State != executorapi.PullRequestOpen {
				t.Errorf("response = %+v, want the open pull request %s", response, tr.Status.Execution.PullRequestURL)
			}
		}
		if prs := env.GitHub.PullRequests(); len(prs) != 1 {
			t.Errorf("pull requests = %+v, want still one", prs)
		}
		if got := env.Sandbox.Rev("refs/heads/" + env.GitHub.PullRequests()[0].Head); got != branchBefore {
			t.Errorf("the branch moved from %s to %s", branchBefore, got)
		}
	})
	t.Run("a merged pull request is returned with its merge commit", func(t *testing.T) {
		merged := env.GitHub.CompleteChecks(t, 1)
		response, err := executor.Execute(t.Context(), proposed.Name, hash)
		if err != nil {
			t.Fatal(err)
		}
		if response.State != executorapi.PullRequestMerged || response.MergeCommit != merged {
			t.Errorf("response = %+v, want merged at %s", response, merged)
		}
	})
	t.Run("another hash is refused", func(t *testing.T) {
		_, err := executor.Execute(t.Context(), proposed.Name, absentHash)
		requireRefused(t, err, http.StatusConflict, executorapi.ReasonHashMismatch)
		if prs := env.GitHub.PullRequests(); len(prs) != 1 {
			t.Errorf("pull requests = %+v, want still one", prs)
		}
	})
}

func TestConcurrentRequestsOpenOnePullRequest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	hash := proposed.Status.Proposal.Hash
	executor := env.ServerExecutorClient(t)

	approve(t, env, proposed)
	var wg sync.WaitGroup
	urls := make([]string, 6)
	for i := range urls {
		wg.Go(func() {
			// A request that arrives before the decision is applied is refused, so ask until it is carried out.
			for ctx := t.Context(); ctx.Err() == nil; time.Sleep(10 * time.Millisecond) {
				if response, err := executor.Execute(ctx, proposed.Name, hash); err == nil {
					urls[i] = response.PullRequestURL
					return
				}
			}
		})
	}
	wg.Wait()
	tr := withPullRequest(t, env, proposed.Name)
	for i, url := range urls {
		if url != tr.Status.Execution.PullRequestURL {
			t.Errorf("request %d got %q, want %q", i, url, tr.Status.Execution.PullRequestURL)
		}
	}
	if prs := env.GitHub.PullRequests(); len(prs) != 1 {
		t.Errorf("pull requests = %+v, want exactly one", prs)
	}
}

func TestExecutionRecoversWhenGitHubFailsAfterTheBranchIsPushed(t *testing.T) {
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

func TestExecutorRefusesWhatIsNotApproved(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	executor := env.ServerExecutorClient(t)

	t.Run("a Proposed Triage", func(t *testing.T) {
		proposed := proposedTriage(t, env, checkout)
		_, err := executor.Execute(t.Context(), proposed.Name, proposed.Status.Proposal.Hash)
		requireRefused(t, err, http.StatusConflict, executorapi.ReasonNotApproved)
		requireNothingDelivered(t, env)
		if requests := env.GitHub.Requests(); len(requests) != 0 {
			t.Errorf("the Executor contacted GitHub before it refused: %v", requests)
		}
		rejection := decision(proposed, v1alpha1.DecisionRejected, "not now")
		if err := approverClient(t, env, aliceUser).Create(t.Context(), rejection); err != nil {
			t.Fatal(err)
		}
		env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseRejected)

		_, err = executor.Execute(t.Context(), proposed.Name, proposed.Status.Proposal.Hash)
		requireRefused(t, err, http.StatusConflict, executorapi.ReasonNotApproved)
		requireNothingDelivered(t, env)
	})
	t.Run("a Triage that does not exist", func(t *testing.T) {
		_, err := executor.Execute(t.Context(), "no-such-triage", absentHash)
		requireRefused(t, err, http.StatusNotFound, executorapi.ReasonTriageNotFound)
	})
	t.Run("a malformed hash", func(t *testing.T) {
		_, err := executor.Execute(t.Context(), "anything", "not-a-hash")
		requireRefused(t, err, http.StatusBadRequest, executorapi.ReasonBadRequest)
	})
}

// TestExecutorChecksTheApprovalItself plays a compromised server: it writes
// Triage status, which only the server may, to claim an Approval that no human gave.
func TestExecutorChecksTheApprovalItself(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	asServer := serverClient(t, env)

	t.Run("a Triage marked Approved without a TriageApproval", func(t *testing.T) {
		proposed := proposedTriage(t, env, checkout)
		writeStatus(t, asServer, proposed.Name, func(tr *v1alpha1.Triage) {
			tr.Status.Phase = v1alpha1.PhaseApproved
			tr.Status.Decision = &v1alpha1.Decision{
				Outcome: v1alpha1.DecisionApproved, Approver: "mallory", DecidedAt: metav1.NewTime(env.Clock.Now()),
			}
		})

		requireFailed(t, env, proposed.Name, v1alpha1.ReasonExecutionRefused, string(executorapi.ReasonNoApproval))
		requireNothingDelivered(t, env)
		if requests := env.GitHub.Requests(); len(requests) != 0 {
			t.Errorf("the Executor contacted GitHub before it refused: %v", requests)
		}
	})
	t.Run("a patch swapped after Validation", func(t *testing.T) {
		proposed := proposedTriage(t, env, checkout)
		writeStatus(t, asServer, proposed.Name, func(tr *v1alpha1.Triage) {
			tr.Status.Proposal.RenderedChange.Patch += "\n"
		})

		// The hash is unchanged, so the human's Approval binds to it, but the stored patch no longer hashes to it.
		approve(t, env, proposed)
		requireFailed(t, env, proposed.Name, v1alpha1.ReasonExecutionRefused, string(executorapi.ReasonProposalAltered))
		requireNothingDelivered(t, env)
	})
}

// renderedBy returns validators whose PatchResources renders the given change
// against the Sandbox's head, whatever the Proposal asks for. They stand in
// for a server whose Validation went wrong, which the Executor must survive.
func renderedBy(path, patch string) testenv.Option {
	return testenv.WithValidators(func(e *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		validators := proposal.DefaultValidators()
		validators[v1alpha1.ActionPatchResources] = proposal.ActionValidatorFunc(
			func(context.Context, proposal.Request) (proposal.Outcome, error) {
				return proposal.Outcome{
					Change: &v1alpha1.RenderedChange{Path: path, Patch: patch},
					Base:   &v1alpha1.ProposalBase{Commit: e.Sandbox.Head()},
				}, nil
			})
		return validators
	})
}

const (
	deploymentPath = "apps/checkout/deployment.yaml"

	// misplacedPatch does not match the file's content.
	misplacedPatch = `diff --git a/apps/checkout/deployment.yaml b/apps/checkout/deployment.yaml
--- a/apps/checkout/deployment.yaml
+++ b/apps/checkout/deployment.yaml
@@ -1,2 +1,2 @@
 a line that the file does not have
-memory: 1Mi
+memory: 2Mi
`

	// twoFilePatch applies cleanly but also changes README.md.
	twoFilePatch = `diff --git a/apps/checkout/deployment.yaml b/apps/checkout/deployment.yaml
--- a/apps/checkout/deployment.yaml
+++ b/apps/checkout/deployment.yaml
@@ -1 +1 @@
-memory: 1Mi
+memory: 2Mi
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-sandbox
+owned
`

	// workflowPatch changes a GitHub workflow, which runs with the repository's credentials.
	workflowPatch = `diff --git a/.github/workflows/check.yaml b/.github/workflows/check.yaml
--- a/.github/workflows/check.yaml
+++ b/.github/workflows/check.yaml
@@ -1 +1 @@
-on: push
+on: pull_request_target
`
)

func TestExecutorRefusesAChangeItMustNotApply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		patch   string
		reason  executorapi.Reason
		message string
	}{
		{"a patch that does not apply cleanly", deploymentPath, misplacedPatch,
			executorapi.ReasonPatchDoesNotApply, "does not apply"},
		{"a patch that also changes another file", deploymentPath, twoFilePatch,
			executorapi.ReasonUnsafeChange, "only " + deploymentPath},
		{"a change to a GitHub workflow", ".github/workflows/check.yaml", workflowPatch,
			executorapi.ReasonUnsafeChange, ".github"},
		{"a path outside the Sandbox", "../outside.yaml", workflowPatch,
			executorapi.ReasonUnsafeChange, "unusable path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.Start(t, renderedBy(tc.path, tc.patch))
			env.Sandbox.Commit(deploymentPath, "memory: 1Mi\n", "Add checkout")
			env.Sandbox.Commit(".github/workflows/check.yaml", "on: push\n", "Add the check")
			name, agent := startTriage(t, env, checkout)
			report := patchReport("app", nil, quantities("memory", "2Mi"))
			if result, err := agent.API.PostReport(t.Context(), report); err != nil || result.StatusCode != http.StatusOK {
				t.Fatalf("report: %+v, %v", result, err)
			}
			proposed := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)

			approve(t, env, proposed)
			requireFailed(t, env, name, v1alpha1.ReasonExecutionRefused, string(tc.reason))
			requireFailed(t, env, name, v1alpha1.ReasonExecutionRefused, tc.message)
			requireNothingDelivered(t, env)
		})
	}
}

func TestExecutorAcceptsOnlyPagerlessServer(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)
	approve(t, env, proposed)
	withPullRequest(t, env, proposed.Name)
	hash := proposed.Status.Proposal.Hash

	cases := []struct {
		name   string
		token  string
		status int
		reason executorapi.Reason
	}{
		{"no token", "", http.StatusUnauthorized, executorapi.ReasonUnauthenticated},
		{"garbage", "not-a-token", http.StatusUnauthorized, executorapi.ReasonUnauthenticated},
		{"the server's token for another audience", env.TokenForAccount(t, "pagerless-server", "pagerless-server"),
			http.StatusUnauthorized, executorapi.ReasonUnauthenticated},
		{"the agent's token for the Executor's audience", env.TokenForAccount(t, "pagerless-agent", executorapi.Audience),
			http.StatusForbidden, executorapi.ReasonForbidden},
		{"the Executor's own token", env.TokenForAccount(t, "pagerless-executor", executorapi.Audience),
			http.StatusForbidden, executorapi.ReasonForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.ExecutorClient(tc.token).Execute(t.Context(), proposed.Name, hash)
			requireRefused(t, err, tc.status, tc.reason)
		})
	}
	t.Run("the server's token", func(t *testing.T) {
		if _, err := env.ServerExecutorClient(t).Execute(t.Context(), proposed.Name, hash); err != nil {
			t.Errorf("pagerless-server was refused: %v", err)
		}
	})
}

func TestExecutorHoldsOnlyReadAccessToTheCluster(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)

	can := func(verb, resource, subresource string) bool {
		return env.Can(t, testenv.ExecutorAccount, nil, verb, pagerlessGroup, resource, subresource)
	}
	for _, resource := range []string{triagesResource, approvalsKind} {
		if !can("get", resource, "") || !can("list", resource, "") {
			t.Errorf("the Executor cannot read %s, which it needs to check the Approval", resource)
		}
	}
	for _, verb := range writeVerbs {
		for _, resource := range []string{triagesResource, approvalsKind} {
			if can(verb, resource, "") {
				t.Errorf("the Executor may %s %s", verb, resource)
			}
		}
		if can(verb, triagesResource, "status") {
			t.Errorf("the Executor may %s triages/status, which only pagerless-server writes", verb)
		}
	}
	if env.Can(t, testenv.ExecutorAccount, nil, "get", "", "secrets", "") {
		t.Errorf("the Executor may read Secrets")
	}
}

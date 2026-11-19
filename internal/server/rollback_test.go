package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// rollbackRefused reports a Rollback Proposal and returns its Validation errors.
func rollbackRefused(t *testing.T, env *testenv.Env, targetName, commit string) []string {
	t.Helper()
	name, agent := startPage(t, env, targetName)
	result, err := agent.API.PostReport(t.Context(), rollbackReport(commit))
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusUnprocessableEntity || result.Report == nil || result.Report.Accepted {
		t.Fatalf("answer = %+v, want 422 with errors", result)
	}
	messages := make([]string, 0, len(result.Report.Errors))
	for _, e := range result.Report.Errors {
		messages = append(messages, e.Field+": "+e.Message)
	}
	if tr := env.Get(t, name); tr.Status.Proposal != nil {
		t.Errorf("a refused Rollback Proposal was stored: %+v", tr.Status.Proposal)
	}
	return messages
}

// requireRefusalContaining checks that one Validation error mentions each part.
func requireRefusalContaining(t *testing.T, messages []string, parts ...string) {
	t.Helper()
	for _, message := range messages {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(message, part)
		}
		if matched {
			return
		}
	}
	t.Errorf("Validation errors = %v, want one containing %v", messages, parts)
}

func TestRollbackRevertsTheCommitThatBrokeTheTarget(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withSandboxActions())
	w := target{name: checkout}
	deploy(t, env, w)
	good := w.manifest()
	broken := strings.Replace(good, "registry.invalid/checkout:1.0", "registry.invalid/checkout:9.9", 1)
	brokenCommit := env.Sandbox.Commit(w.sourcePath(), broken, "Publish an image tag that does not exist")
	head := env.Sandbox.Head()

	name, agent := startPage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), rollbackReport(brokenCommit))
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK || result.Report == nil || result.Report.Phase != v1alpha1.PhaseProposed {
		t.Fatalf("answer = %+v, want 200 and phase Proposed", result)
	}

	tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
	p := tr.Status.Proposal
	if p.Action != v1alpha1.ActionRollback || p.Parameters == nil || p.Parameters.Rollback == nil {
		t.Fatalf("proposal = %+v, want the Rollback Action with its parameters", p)
	}
	if got := p.Parameters.Rollback.Commit; got != brokenCommit {
		t.Errorf("rollback commit = %s, want the requested %s", got, brokenCommit)
	}
	change := p.RenderedChange
	if change == nil || change.Path != w.sourcePath() {
		t.Fatalf("rendered change = %+v, want the revert of %s", change, w.sourcePath())
	}
	// The patch is the exact revert of the one commit the Proposal names.
	for _, want := range []string{
		"--- a/" + w.sourcePath(), "+++ b/" + w.sourcePath(),
		"-          image: registry.invalid/checkout:9.9", "+          image: registry.invalid/checkout:1.0",
	} {
		if !strings.Contains(change.Patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, change.Patch)
		}
	}
	if err := gitApplyCheck(t, env, change.Patch); err != nil {
		t.Errorf("the revert does not apply to the Sandbox: %v", err)
	}
	if p.Base == nil || p.Base.Commit != head {
		t.Errorf("base = %+v, want the current Sandbox commit %s", p.Base, head)
	}
	want, err := proposal.Hash(tr, p.ProposedAction, p.RenderedChange, p.Base)
	if err != nil || p.Hash != want {
		t.Errorf("hash = %s, want %s (error %v): it covers the Page, Action, Target, and rendered revert",
			p.Hash, want, err)
	}
	if env.Sandbox.Head() != head {
		t.Errorf("Validation moved the Sandbox head from %s to %s", head, env.Sandbox.Head())
	}

	// The pull request engine applies the stored revert by pull request, as for any Proposal.
	approve(t, env, tr)
	executing := withPullRequest(t, env, tr.Name)
	prs := env.GitHub.PullRequests()
	if len(prs) != 1 {
		t.Fatalf("pull requests = %+v, want exactly one", prs)
	}
	branch := "refs/heads/" + prs[0].Head
	if parent := env.Sandbox.Rev(branch + "^"); parent != head {
		t.Errorf("parent of the revert commit = %s, want the base %s", parent, head)
	}
	if got := env.Sandbox.Diff(head, branch); got != change.Patch {
		t.Errorf("the branch differs from the stored revert:\n%s\nwant:\n%s", got, change.Patch)
	}
	merged := env.GitHub.CompleteChecks(t, prs[0].Number)
	env.WaitFor(t, tr.Name, "the merge commit is recorded", func(latest *v1alpha1.Page) bool {
		return latest.Status.PullRequest != nil && latest.Status.PullRequest.MergeCommit != ""
	})
	if got := env.Sandbox.File("main", w.sourcePath()); got != good {
		t.Errorf("Sandbox main after the merge = %q, want the reverted manifest", got)
	}

	// Verification decides the outcome unchanged.
	verifyingPage := verifying(t, env, executing, merged)
	env.Clock.Step(5 * time.Minute)
	recovered := env.WaitForPhase(t, verifyingPage.Name, v1alpha1.PhaseRecovered)
	requireChecks(t, recovered, map[string]bool{
		v1alpha1.CheckArgoCDSynced:     true,
		v1alpha1.CheckRolloutAvailable: true,
		v1alpha1.CheckNoNewRestarts:    true,
		v1alpha1.CheckAlertResolved:    true,
	})
}

func TestRollbackRevertsTheCommitThatBrokeADeclaredDependency(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withSandboxActions())
	dependency := "shared/checkout-config.yaml"
	env.Sandbox.Commit(dependency, "payment: live\n", "Add the checkout configuration")
	w := target{name: checkout, dependsOn: []string{dependency}}
	deploy(t, env, w)
	broken := env.Sandbox.Commit(dependency, "payment: broken\n", "Break the shared checkout configuration")

	name, agent := startPage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), rollbackReport(broken))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
	change := tr.Status.Proposal.RenderedChange
	if change == nil || change.Path != dependency {
		t.Fatalf("rendered change = %+v, want the revert of the declared dependency %s", change, dependency)
	}
	for _, want := range []string{
		"--- a/" + dependency, "+++ b/" + dependency, "-payment: broken", "+payment: live",
	} {
		if !strings.Contains(change.Patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, change.Patch)
		}
	}
	if err := gitApplyCheck(t, env, change.Patch); err != nil {
		t.Errorf("the revert does not apply to the Sandbox: %v", err)
	}

	approve(t, env, tr)
	withPullRequest(t, env, tr.Name)
	env.GitHub.CompleteChecks(t, 1)
	env.WaitFor(t, tr.Name, "the merge commit is recorded", func(latest *v1alpha1.Page) bool {
		return latest.Status.PullRequest != nil && latest.Status.PullRequest.MergeCommit != ""
	})
	if got := env.Sandbox.File("main", dependency); got != "payment: live\n" {
		t.Errorf("Sandbox main after the merge = %q, want the reverted configuration", got)
	}
}

func TestRollbackRefusesACommitOutsideTheAllowedPaths(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withSandboxActions())
	const otherPath = "apps/other/deployment.yaml"

	type refusal struct {
		name   string
		target string
		commit string
		parts  []string
	}
	twoFiles := target{name: "two-files"}
	cases := []refusal{
		{name: "a commit that touches another Target", target: "other-path",
			parts: []string{"last 20", "commits"}},
		{name: "a commit that is not in the Target Repository", target: "absent-commit",
			parts: []string{"commit", "not in the Target Repository"}},
		{name: "a commit that touches more than one file", target: twoFiles.name,
			parts: []string{"more than one file"}},
	}
	// The Sandbox's working copy is shared, so every Target and every commit
	// is made before the parallel subtests report their Proposals.
	for i := range cases {
		deploy(t, env, target{name: cases[i].target})
	}
	commits := []string{
		env.Sandbox.Commit(otherPath, "kind: Deployment\n", "Change another Target"),
		"0123456789abcdef0123456789abcdef01234567",
		env.Sandbox.CommitAll(map[string]string{
			twoFiles.sourcePath():  strings.Replace(twoFiles.manifest(), ":1.0", ":9.9", 1),
			"shared/checkout.yaml": "payment: broken\n",
		}, "Change the Target and a shared file together"),
	}
	for i := range cases {
		cases[i].commit = commits[i]
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			messages := rollbackRefused(t, env, c.target, c.commit)
			requireRefusalContaining(t, messages, c.parts...)
		})
	}
}

func TestRollbackWindowIsTheLast20CommitsTouchingTheAllowedPaths(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withSandboxActions())

	// Each Target declares 21 dependency paths, which one commit creates and
	// 21 later commits revise one at a time, so that every revision touches an
	// allowed path exactly once and reverting any of them applies cleanly.
	revisions := 21
	commits := make(map[string][]string, 2)
	for _, name := range []string{"window-refused", "window-accepted"} {
		dependsOn := make([]string, revisions)
		files := make(map[string]string, revisions)
		for i := range dependsOn {
			dependsOn[i] = fmt.Sprintf("shared/%s-%d.yaml", name, i)
			files[dependsOn[i]] = "revision: initial\n"
		}
		w := target{name: name, dependsOn: dependsOn}
		deploy(t, env, w)
		env.Sandbox.CommitAll(files, "Add the shared configuration of "+name)
		for i, path := range dependsOn {
			commits[name] = append(commits[name], env.Sandbox.Commit(
				path, fmt.Sprintf("revision: %d\n", i), fmt.Sprintf("Revise %s, %d", name, i)))
		}
	}

	t.Run(fmt.Sprintf("the %dst newest commit is refused", revisions), func(t *testing.T) {
		t.Parallel()
		messages := rollbackRefused(t, env, "window-refused", commits["window-refused"][0])
		requireRefusalContaining(t, messages, "last 20", "commits")
	})
	t.Run("the 20th newest commit is accepted", func(t *testing.T) {
		t.Parallel()
		name, agent := startPage(t, env, "window-accepted")
		commit := commits["window-accepted"][1]
		result, err := agent.API.PostReport(t.Context(), rollbackReport(commit))
		if err != nil || result.StatusCode != http.StatusOK {
			t.Fatalf("report: %+v (%+v), %v, want 200", result, result.Report, err)
		}
		tr := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
		change := tr.Status.Proposal.RenderedChange
		if want := "shared/window-accepted-1.yaml"; change == nil || change.Path != want {
			t.Fatalf("rendered change = %+v, want the revert of %s", change, want)
		}
		for _, want := range []string{"-revision: 1", "+revision: initial"} {
			if !strings.Contains(change.Patch, want) {
				t.Errorf("patch lacks %q:\n%s", want, change.Patch)
			}
		}
	})
}

func TestRollbackProposalBecomesStaleWhenTheSandboxMoves(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withSandboxActions(), testenv.WithProposalPollInterval(proposalPoll))
	w := target{name: checkout}
	deploy(t, env, w)
	broken := strings.Replace(w.manifest(), "registry.invalid/checkout:1.0", "registry.invalid/checkout:9.9", 1)
	commit := env.Sandbox.Commit(w.sourcePath(), broken, "Publish an image tag that does not exist")

	name, agent := startPage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), rollbackReport(commit))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	proposed := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)

	t.Run("an unrelated commit does not make it stale", func(t *testing.T) {
		env.Sandbox.Commit("apps/other/deployment.yaml", "kind: Deployment\n", "Change another Target")
		time.Sleep(20 * proposalPoll)
		after := env.Get(t, name)
		if after.Status.Proposal.Hash != proposed.Status.Proposal.Hash {
			t.Errorf("an unrelated commit re-validated the Rollback Proposal: %+v", after.Status)
		}
	})

	t.Run("editing the reverted file re-renders the revert", func(t *testing.T) {
		head := env.Sandbox.Commit(w.sourcePath(), broken+"# edited in the Sandbox\n", "Edit the manifest")
		after := revalidated(t, env, name, proposed.Status.Proposal.Hash)
		if got := after.Status.Proposal.RenderedChange.Path; got != w.sourcePath() {
			t.Errorf("rendered path = %s, want %s", got, w.sourcePath())
		}
		if got := after.Status.Proposal.Base.Commit; got != head {
			t.Errorf("base commit = %s, want the new head %s", got, head)
		}
		if err := gitApplyCheck(t, env, after.Status.Proposal.RenderedChange.Patch); err != nil {
			t.Errorf("the re-rendered revert does not apply to the Sandbox: %v", err)
		}
	})
}

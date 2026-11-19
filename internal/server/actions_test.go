package server_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// withRemediations starts the harness with Validation accepting every
// executable Action for the sandbox namespace.
func withRemediations() testenv.Option {
	return testenv.WithValidators(func(e *testenv.Env) map[v1alpha1.Action]proposal.ActionValidator {
		return e.Remediations(sandboxNamespace)
	})
}

// scaleReport is a report whose Proposal scales the Workload.
func scaleReport(replicas int32) agentapi.Report {
	return agentapi.Report{
		Diagnosis: "The Workload is at its capacity, so it needs more replicas.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal: v1alpha1.ProposedAction{
			Action:     v1alpha1.ActionScale,
			Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: replicas}},
		},
	}
}

// changedLines returns the removed and added content lines of a patch, without
// its file headers.
func changedLines(patch string) (removed, added []string) {
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line)
		case strings.HasPrefix(line, "+"):
			added = append(added, line)
		}
	}
	return removed, added
}

func TestScaleProposalRendersTheReplicaCountPatchAndDeliversItByPullRequest(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withRemediations())
	w := workload{name: checkout}
	deploy(t, env, w)
	head := env.Sandbox.Head()
	before := liveDeployment(t, env, checkout)

	name, agent := startTriage(t, env, checkout)
	result, err := agent.API.PostReport(t.Context(), scaleReport(3))
	if err != nil || result.StatusCode != http.StatusOK || result.Report.Phase != v1alpha1.PhaseProposed {
		t.Fatalf("report: %+v, %v, want 200 and phase Proposed", result, err)
	}
	proposed := env.WaitForPhase(t, name, v1alpha1.PhaseProposed)
	p := proposed.Status.Proposal
	if p.Action != v1alpha1.ActionScale || p.Parameters == nil || p.Parameters.Scale == nil ||
		p.Parameters.Scale.Replicas != 3 {
		t.Fatalf("proposal = %+v, want Scale to 3 replicas", p)
	}
	change := p.RenderedChange
	if change == nil || change.Path != w.sourcePath() {
		t.Fatalf("rendered change = %+v, want the patch for %s", change, w.sourcePath())
	}
	removed, added := changedLines(change.Patch)
	if !slices.Equal(removed, []string{"-  replicas: 1"}) || !slices.Equal(added, []string{"+  replicas: 3"}) {
		t.Errorf("the patch changes more than the replica count:\n%s", change.Patch)
	}
	if err := gitApplyCheck(t, env, change.Patch); err != nil {
		t.Errorf("the patch does not apply to the Sandbox: %v", err)
	}
	if p.Base == nil || p.Base.Commit != head || p.Base.WorkloadGeneration != before.Generation {
		t.Errorf("base = %+v, want commit %s and generation %d", p.Base, head, before.Generation)
	}
	wantHash, err := proposal.Hash(proposed, p.ProposedAction, p.RenderedChange, p.Base)
	if err != nil || p.Hash != wantHash {
		t.Errorf("hash = %s, want %s (error %v)", p.Hash, wantHash, err)
	}

	// Validation renders the change and dry-runs it. It changes nothing.
	after := liveDeployment(t, env, checkout)
	if after.Generation != before.Generation || *after.Spec.Replicas != 1 {
		t.Errorf("the dry run changed the Workload: generation %d, replicas %d",
			after.Generation, *after.Spec.Replicas)
	}
	if env.Sandbox.Head() != head {
		t.Errorf("Validation moved the Sandbox head from %s to %s", head, env.Sandbox.Head())
	}

	// The Executor delivers the change exactly as it delivers PatchResources.
	approve(t, env, proposed)
	tr := withPullRequest(t, env, proposed.Name)
	prs := env.GitHub.PullRequests()
	if len(prs) != 1 || prs[0].URL != tr.Status.Execution.PullRequestURL {
		t.Fatalf("pull requests = %+v, want exactly the recorded one", prs)
	}
	if got := env.Sandbox.Diff(p.Base.Commit, "refs/heads/"+prs[0].Head); got != change.Patch {
		t.Errorf("the branch differs from the stored patch:\n%s\nwant:\n%s", got, change.Patch)
	}
	if env.Sandbox.Head() != head {
		t.Errorf("Sandbox main moved before the pull request merged")
	}

	// The existing Verification path takes over once the pull request merges.
	commit := env.GitHub.CompleteChecks(t, prs[0].Number)
	env.WaitFor(t, name, "the merge commit is recorded", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Execution != nil && tr.Status.Execution.MergeCommit != ""
	})
	env.MarkDeploymentAvailable(t, sandboxNamespace, checkout)
	env.ArgoCD.Sync(commit)
	env.WaitForPhase(t, name, v1alpha1.PhaseVerifying)
	env.Clock.Step(5 * time.Minute)
	recovered := env.WaitForPhase(t, name, v1alpha1.PhaseRecovered)
	requireChecks(t, recovered, map[string]bool{
		v1alpha1.CheckArgoCDSynced:     true,
		v1alpha1.CheckRolloutAvailable: true,
		v1alpha1.CheckNoNewRestarts:    true,
		v1alpha1.CheckAlertResolved:    true,
	})
	if !strings.Contains(env.Sandbox.File("main", w.sourcePath()), "replicas: 3") {
		t.Errorf("Sandbox main does not hold the approved replica count after the merge")
	}
}

func TestScaleProposalsOutsideTheBoundsAreRefused(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withRemediations())
	cases := []struct {
		name     string
		replicas int32
		workload workload
	}{
		{name: "below the minimum", replicas: 0},
		{name: "above the maximum", replicas: 11},
	}
	for i := range cases {
		cases[i].workload = workload{name: "scaled-" + strings.ReplaceAll(cases[i].name, " ", "-")}
		deploy(t, env, cases[i].workload)
	}
	head := env.Sandbox.Head()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			created := env.CreateTriage(t, testenv.NewTriage(tc.workload.name))
			agent := env.StartAgent(t, created.Name)
			result, err := agent.API.PostReport(t.Context(), scaleReport(tc.replicas))
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != http.StatusUnprocessableEntity || result.Report == nil || result.Report.Accepted {
				t.Fatalf("answer = %+v, want 422 with errors", result)
			}
			found := false
			for _, e := range result.Report.Errors {
				found = found || (e.Field == "proposal.parameters.scale.replicas" &&
					strings.Contains(e.Message, permittedRangeText))
			}
			if !found {
				t.Errorf("errors = %+v, want one for proposal.parameters.scale.replicas mentioning the permitted range",
					result.Report.Errors)
			}
			tr := env.Get(t, created.Name)
			if tr.Status.Proposal != nil || tr.Status.ValidationAttempts != 1 {
				t.Errorf("proposal = %+v, validationAttempts = %d, want no proposal and one failed attempt",
					tr.Status.Proposal, tr.Status.ValidationAttempts)
			}
			if tr.Status.Phase != v1alpha1.PhaseInvestigating {
				t.Errorf("phase = %s, want Investigating so the agent can revise", tr.Status.Phase)
			}
		})
	}

	requireNothingDelivered(t, env)
	if got := env.Sandbox.Head(); got != head {
		t.Errorf("a refused Scale moved the Sandbox head from %s to %s", head, got)
	}
	// Nothing changed the live Workload either.
	for _, tc := range cases {
		d := liveDeployment(t, env, tc.workload.name)
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
			t.Errorf("Deployment %s has replicas %v, want 1", tc.workload.name, d.Spec.Replicas)
		}
	}
}

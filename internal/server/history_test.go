package server_test

import (
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// podsLogTool is the read-only Kubernetes MCP tool that the tests report as a tool call.
const podsLogTool = "kubernetes.pods_log"

// recovered drives one Page from creation to Recovered. Its report carries the usage that the metrics tests
// assert, and its Proposal patches the checkout Target's memory limit.
func recovered(t *testing.T, env *testenv.Env) string {
	t.Helper()
	deploy(t, env, target{name: checkout})
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), withUsage(
		patchReport("app", nil, quantities("memory", "512Mi")), reportUsage,
	))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	approve(t, env, proposed)
	withPullRequest(t, env, created.Name)
	commit := env.GitHub.CompleteChecks(t, 1)
	tr := env.WaitFor(t, created.Name, "the merge commit is recorded", func(tr *v1alpha1.Page) bool {
		return tr.Status.PullRequest != nil && tr.Status.PullRequest.MergeCommit != ""
	})
	verifying(t, env, tr, commit)
	env.Clock.Step(5 * time.Minute)
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseRecovered)
	return created.Name
}

func TestHistoryRecordsEveryPhaseChange(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	name := recovered(t, env)

	// The server writes the snapshot after it writes the status, so the admin client can observe Recovered before
	// the history holds it. Wait for the terminal snapshot rather than racing the recorder.
	eventually(t, "history records the terminal snapshot", func() bool {
		snapshots, err := env.History.Snapshots(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		return len(snapshots) > 0 && snapshots[len(snapshots)-1].Phase == v1alpha1.PhaseRecovered
	})
	snapshots, err := env.History.Snapshots(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	phases := make([]v1alpha1.Phase, 0, len(snapshots))
	for _, snapshot := range snapshots {
		phases = append(phases, snapshot.Phase)
		if snapshot.Status.Phase != snapshot.Phase {
			t.Errorf("snapshot phase = %s, status phase = %s", snapshot.Phase, snapshot.Status.Phase)
		}
		if snapshot.Page != name || snapshot.Namespace != testenv.Namespace {
			t.Errorf("snapshot names %s/%s, want %s/%s",
				snapshot.Namespace, snapshot.Page, testenv.Namespace, name)
		}
	}
	want := []v1alpha1.Phase{
		v1alpha1.PhasePending, v1alpha1.PhaseInvestigating, v1alpha1.PhaseProposed,
		v1alpha1.PhaseApproved, v1alpha1.PhasePullRequestOpen, v1alpha1.PhaseVerifying,
		v1alpha1.PhaseRecovered,
	}
	if !slices.Equal(phases, want) {
		t.Fatalf("snapshot phases = %v, want exactly one snapshot per Phase change %v", phases, want)
	}

	// The Proposed snapshot holds the Proposal and the usage that the report declared.
	proposed := snapshots[2]
	if proposed.Status.Proposal == nil || proposed.Status.Proposal.Action != v1alpha1.ActionPatchResources {
		t.Errorf("Proposed snapshot proposal = %+v, want the PatchResources Proposal", proposed.Status.Proposal)
	}
	if proposed.Status.Usage == nil || *proposed.Status.Usage != reportUsage {
		t.Errorf("Proposed snapshot usage = %+v, want %+v", proposed.Status.Usage, reportUsage)
	}

	// The terminal snapshot records both lifecycle times, taken from the server's clock.
	terminal := snapshots[len(snapshots)-1]
	if terminal.Status.StartedAt == nil || !terminal.Status.StartedAt.Time.Equal(testenv.StartTime) {
		t.Errorf("startedAt = %v, want %v", terminal.Status.StartedAt, testenv.StartTime)
	}
	wantTerminal := testenv.StartTime.Add(5 * time.Minute)
	if terminal.Status.TerminalAt == nil || !terminal.Status.TerminalAt.Time.Equal(wantTerminal) {
		t.Errorf("terminalAt = %v, want %v", terminal.Status.TerminalAt, wantTerminal)
	}
}

func TestHistoryAppendsEveryStepEvent(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)

	finding := logFinding
	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindFinding, Summary: "recorded a log finding", Finding: &finding,
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the Finding event: %+v, %v, want 202", result, err)
	}
	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindToolCall, Tool: podsLogTool, Summary: "read the previous logs",
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the ToolCall event: %+v, %v, want 202", result, err)
	}

	steps, err := env.History.Steps(t.Context(), created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("recorded %d steps, want 2", len(steps))
	}
	if steps[0].Kind != v1alpha1.StepKindFinding || steps[0].Finding == nil || *steps[0].Finding != finding {
		t.Errorf("first step = %+v, want the Finding it carried", steps[0])
	}
	if steps[1].Kind != v1alpha1.StepKindToolCall || steps[1].Tool != podsLogTool {
		t.Errorf("second step = %+v, want the ToolCall", steps[1])
	}

	// Status keeps the most recent MaxSteps steps; history keeps every one of them.
	total := 2 + v1alpha1.MaxSteps + 5
	for i := range v1alpha1.MaxSteps + 5 {
		result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
			Kind: v1alpha1.StepKindProgress, Summary: "progress",
		})
		if err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post progress %d: %+v, %v, want 202", i, result, err)
		}
	}
	steps, err = env.History.Steps(t.Context(), created.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != total {
		t.Errorf("history recorded %d steps, want all %d", len(steps), total)
	}
	if got := len(env.Get(t, created.Name).Status.Steps); got != v1alpha1.MaxSteps {
		t.Errorf("status keeps %d steps, want the bounded %d", got, v1alpha1.MaxSteps)
	}
}

func TestHistoryCannotBeRewritten(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindProgress, Summary: "progress",
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post an event: %+v, %v, want 202", result, err)
	}

	db, err := sql.Open("sqlite", "file:"+env.HistoryPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		"UPDATE page_snapshots SET phase = 'Recovered'",
		"DELETE FROM page_snapshots",
		"UPDATE page_steps SET summary = 'rewritten'",
		"DELETE FROM page_steps",
	} {
		if _, err := db.ExecContext(t.Context(), statement); err == nil ||
			!strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: error = %v, want an append-only refusal", statement, err)
		}
	}
}

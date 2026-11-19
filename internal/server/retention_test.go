package server_test

import (
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// retentionPollBound is longer than the harness's retention poll interval, so a test that asserts a Page still
// exists has given the controller several chances to delete it.
const retentionPollBound = 500 * time.Millisecond

func TestRetentionDeletesATerminalPageAfterSevenDays(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithRetentionPollInterval(100*time.Millisecond))
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	result, err := agent.API.PostReport(t.Context(), withUsage(noActionReport(), reportUsage))
	if err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	approve(t, env, proposed)
	diagnosed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseDiagnosed)
	if diagnosed.Status.TerminalAt == nil || !diagnosed.Status.TerminalAt.Time.Equal(testenv.StartTime) {
		t.Fatalf("terminalAt = %v, want the clock's time %v", diagnosed.Status.TerminalAt, testenv.StartTime)
	}
	// The recorder writes the terminal snapshot after the status, so wait for it before counting history.
	eventually(t, "history records the Diagnosed snapshot", func() bool {
		snapshots, err := env.History.Snapshots(t.Context(), created.Name)
		if err != nil {
			t.Fatal(err)
		}
		return len(snapshots) > 0 && snapshots[len(snapshots)-1].Phase == v1alpha1.PhaseDiagnosed
	})
	before, err := env.History.Snapshots(t.Context(), created.Name)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("six days and twenty-three hours is not seven days", func(t *testing.T) {
		env.Clock.Step(6*24*time.Hour + 23*time.Hour)
		time.Sleep(retentionPollBound)
		var live v1alpha1.Page
		key := types.NamespacedName{Namespace: testenv.Namespace, Name: created.Name}
		if err := env.Client.Get(t.Context(), key, &live); err != nil {
			t.Fatalf("the Page was deleted before seven days: %v", err)
		}
	})

	t.Run("one more hour deletes it and keeps its history", func(t *testing.T) {
		env.Clock.Step(time.Hour)
		env.WaitForGone(t, created.Name)
		after, err := env.History.Snapshots(t.Context(), created.Name)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("history has %d snapshots after deletion, want the %d it had before", len(after), len(before))
		}
		if last := after[len(after)-1]; last.Phase != v1alpha1.PhaseDiagnosed {
			t.Errorf("last recorded phase = %s, want %s", last.Phase, v1alpha1.PhaseDiagnosed)
		}
	})
}

func TestRetentionKeepsAnActivePage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	env.Clock.Step(8 * 24 * time.Hour)
	time.Sleep(retentionPollBound)

	var live v1alpha1.Page
	key := types.NamespacedName{Namespace: testenv.Namespace, Name: created.Name}
	if err := env.Client.Get(t.Context(), key, &live); err != nil {
		t.Fatalf("an Active Page was deleted: %v", err)
	}
	if live.Status.Phase != v1alpha1.PhaseInvestigating {
		t.Errorf("phase = %s, want Investigating", live.Status.Phase)
	}
}

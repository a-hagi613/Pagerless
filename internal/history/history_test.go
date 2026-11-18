package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// startTime is a deterministic creation time for the records the tests append.
var startTime = time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)

// newTriage returns a Triage with the spec and status that the tests record.
func newTriage(name string, created time.Time) *v1alpha1.Triage {
	tr := &v1alpha1.Triage{
		Spec: v1alpha1.TriageSpec{
			Workload: v1alpha1.WorkloadReference{
				Kind:      v1alpha1.WorkloadKindDeployment,
				Namespace: "sandbox",
				Name:      "checkout",
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: "checkout-abc"},
		},
		Status: v1alpha1.TriageStatus{Phase: v1alpha1.PhasePending},
	}
	tr.Name = name
	tr.Namespace = "pagerless"
	tr.UID = types.UID("uid-" + name)
	tr.CreationTimestamp = metav1.NewTime(created)
	return tr
}

// openVersion1 writes a history database with the schema version 1 layout and one snapshot, as a server built
// before the dashboard recorded specs and creation times left it.
func openVersion1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", dataSourceName(path, false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	status, err := json.Marshal(v1alpha1.TriageStatus{Phase: v1alpha1.PhaseRecovered})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE triage_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			triage_uid TEXT NOT NULL,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			phase TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			status TEXT NOT NULL
		)`,
		`CREATE TABLE triage_steps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			triage_uid TEXT NOT NULL,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			recorded_at TEXT NOT NULL,
			kind TEXT NOT NULL,
			tool TEXT NOT NULL,
			summary TEXT NOT NULL,
			finding TEXT
		)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO triage_snapshots (triage_uid, namespace, name, phase, observed_at, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"uid-old", "pagerless", "triage-old", string(v1alpha1.PhaseRecovered),
		formatTime(startTime.Add(time.Minute)), string(status),
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenUpgradesAVersion1DatabaseInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	openVersion1(t, path)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open a version 1 history: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the history: %v", err)
		}
	})

	snapshots, err := store.Snapshots(context.Background(), "triage-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want the version 1 row", len(snapshots))
	}
	if snapshots[0].Phase != v1alpha1.PhaseRecovered {
		t.Errorf("phase = %s, want Recovered", snapshots[0].Phase)
	}
	if !snapshots[0].CreatedAt.IsZero() || snapshots[0].Spec.Workload.Name != "" {
		t.Errorf("version 1 row gained a creation time or spec: %+v", snapshots[0])
	}

	var version int
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
}

func TestAppendedSnapshotsCarryTheSpecAndCreationTime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the history: %v", err)
		}
	})

	older := newTriage("triage-older", startTime)
	if err := store.AppendSnapshot(context.Background(), older, startTime); err != nil {
		t.Fatal(err)
	}
	newer := newTriage("triage-newer", startTime.Add(time.Minute))
	newer.Status.Phase = v1alpha1.PhaseProposed
	if err := store.AppendSnapshot(context.Background(), newer, startTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A second snapshot of the older Triage must not hide the newer Triage from Latest.
	older.Status.Phase = v1alpha1.PhaseInvestigating
	if err := store.AppendSnapshot(context.Background(), older, startTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	latest, err := store.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest snapshots = %d, want one per Triage", len(latest))
	}
	if latest[0].Triage != "triage-older" || latest[1].Triage != "triage-newer" {
		t.Errorf("order = [%s %s], want the most recently appended first", latest[0].Triage, latest[1].Triage)
	}
	if latest[1].Phase != v1alpha1.PhaseProposed || latest[1].Status.Phase != v1alpha1.PhaseProposed {
		t.Errorf("newer snapshot phase = %s, status phase = %s, want Proposed",
			latest[1].Phase, latest[1].Status.Phase)
	}
	if latest[1].Spec.Workload != newer.Spec.Workload || latest[1].Spec.Trigger != newer.Spec.Trigger {
		t.Errorf("spec = %+v, want %+v", latest[1].Spec, newer.Spec)
	}
	if !latest[1].CreatedAt.Equal(startTime.Add(time.Minute)) {
		t.Errorf("createdAt = %v, want %v", latest[1].CreatedAt, startTime.Add(time.Minute))
	}
}

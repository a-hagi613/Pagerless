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

// newPage returns a Page with the spec and status that the tests record.
func newPage(name string, created time.Time) *v1alpha1.Page {
	tr := &v1alpha1.Page{
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				Kind:      v1alpha1.TargetKindDeployment,
				Namespace: "sandbox",
				Name:      "checkout",
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: testPage},
		},
		Status: v1alpha1.PageStatus{Phase: v1alpha1.PhasePending},
	}
	tr.Name = name
	tr.Namespace = testNamespace
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
	status, err := json.Marshal(v1alpha1.PageStatus{Phase: v1alpha1.PhaseRecovered})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE page_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			page_uid TEXT NOT NULL,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			phase TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			status TEXT NOT NULL
		)`,
		`CREATE TABLE page_steps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			page_uid TEXT NOT NULL,
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
		`INSERT INTO page_snapshots (page_uid, namespace, name, phase, observed_at, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"uid-old", testNamespace, "page-old", string(v1alpha1.PhaseRecovered),
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

	snapshots, err := store.Snapshots(context.Background(), "page-old")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want the version 1 row", len(snapshots))
	}
	if snapshots[0].Phase != v1alpha1.PhaseRecovered {
		t.Errorf("phase = %s, want Recovered", snapshots[0].Phase)
	}
	if !snapshots[0].CreatedAt.IsZero() || snapshots[0].Spec.Target.Name != "" {
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

	older := newPage("page-older", startTime)
	if err := store.AppendSnapshot(context.Background(), older, startTime); err != nil {
		t.Fatal(err)
	}
	newer := newPage("page-newer", startTime.Add(time.Minute))
	newer.Status.Phase = v1alpha1.PhaseProposed
	if err := store.AppendSnapshot(context.Background(), newer, startTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A second snapshot of the older Page must not hide the newer Page from Latest.
	older.Status.Phase = v1alpha1.PhaseInvestigating
	if err := store.AppendSnapshot(context.Background(), older, startTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	latest, err := store.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest snapshots = %d, want one per Page", len(latest))
	}
	if latest[0].Page != "page-older" || latest[1].Page != "page-newer" {
		t.Errorf("order = [%s %s], want the most recently appended first", latest[0].Page, latest[1].Page)
	}
	if latest[1].Phase != v1alpha1.PhaseProposed || latest[1].Status.Phase != v1alpha1.PhaseProposed {
		t.Errorf("newer snapshot phase = %s, status phase = %s, want Proposed",
			latest[1].Phase, latest[1].Status.Phase)
	}
	if latest[1].Spec.Target != newer.Spec.Target || latest[1].Spec.Trigger != newer.Spec.Trigger {
		t.Errorf("spec = %+v, want %+v", latest[1].Spec, newer.Spec)
	}
	if !latest[1].CreatedAt.Equal(startTime.Add(time.Minute)) {
		t.Errorf("createdAt = %v, want %v", latest[1].CreatedAt, startTime.Add(time.Minute))
	}
}

// TestOpenAddsTheRuntimeTablesToAVersion2Database proves the version 3 change is
// strictly additive: a file written by a version 2 server keeps every row and
// gains the Cooldown tables on the next start, without a rewrite.
func TestOpenAddsTheRuntimeTablesToAVersion2Database(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open the history: %v", err)
	}
	if err := store.AppendSnapshot(context.Background(), newPage("page-old", startTime), startTime); err != nil {
		t.Fatalf("append a snapshot: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the history: %v", err)
	}
	// Rewind the file to the version 2 shape: the two append-only tables with
	// their rows, and no runtime tables.
	db, err := sql.Open("sqlite", dataSourceName(path, false))
	if err != nil {
		t.Fatalf("open the raw database: %v", err)
	}
	for _, statement := range []string{
		`DROP TABLE failure_cooldowns`,
		`DROP TABLE failure_observations`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the raw database: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open a version 2 history: %v", err)
	}
	t.Cleanup(func() {
		if err := upgraded.Close(); err != nil {
			t.Errorf("close the history: %v", err)
		}
	})
	snapshots, err := upgraded.Snapshots(context.Background(), "page-old")
	if err != nil {
		t.Fatalf("read the version 2 snapshot: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Phase != v1alpha1.PhasePending {
		t.Fatalf("snapshots = %+v, want the version 2 row unchanged", snapshots)
	}
	for _, table := range []string{"failure_cooldowns", "failure_observations"} {
		if !tableExists(t, upgraded.db, table) {
			t.Errorf("%s is missing after the version 2 upgrade", table)
		}
	}
	var version int
	if err := upgraded.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
	// A second open is a no-op rather than an error, so a restart is safe.
	if err := upgraded.migrate(context.Background()); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
}

// tableExists reports whether a table is present.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	return count == 1
}

// TestRevokedSessionsAreRecordedAndPrunedOnExpiry proves that logout revokes a
// jti for the cookie's remaining lifetime and that an expired row is pruned.
func TestRevokedSessionsAreRecordedAndPrunedOnExpiry(t *testing.T) {
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

	ctx := context.Background()
	revoked, err := store.IsSessionRevoked(ctx, "jti-1", startTime)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("jti-1 reported as revoked before RevokeSession")
	}

	if err := store.RevokeSession(ctx, "jti-1", startTime, startTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	revoked, err = store.IsSessionRevoked(ctx, "jti-1", startTime.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("jti-1 not reported as revoked inside its expiry window")
	}

	// Once the session's expiry passes, IsSessionRevoked returns false and a subsequent write prunes the row.
	revoked, err = store.IsSessionRevoked(ctx, "jti-1", startTime.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Fatal("jti-1 still reported as revoked after its expiry")
	}
	if err := store.RevokeSession(ctx, "jti-2", startTime.Add(2*time.Hour), startTime.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM revoked_sessions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("revoked_sessions row count = %d, want 1 after pruning expired rows", count)
	}
}

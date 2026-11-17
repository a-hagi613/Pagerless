// Package history is the append-only SQLite record of what Pagerless observed. It is written by
// pagerless-server on every Phase change and for every step event, and is never read to decide anything about an
// Active Triage: the Triage resource remains the source of truth (ADR-0001).
//
// Schema version 1 has two tables, each with a trigger that refuses UPDATE and a trigger that refuses DELETE:
//
//	triage_snapshots(id, triage_uid, namespace, name, phase, observed_at, status)
//	triage_steps(id, triage_uid, namespace, name, recorded_at, kind, tool, summary, finding)
//
// A snapshot stores the whole Triage status as JSON next to the phase and the time it was observed. A step stores
// the bounded step the server accepted, with its Finding as JSON when the step carried one. History therefore
// cannot be rewritten by the application.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	// The pure-Go SQLite driver registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// SchemaVersion is the version of the schema that this package creates and reads.
const SchemaVersion = 1

const (
	// snapshotsTable holds one row per Phase change.
	snapshotsTable = "triage_snapshots"
	// stepsTable holds one row per step event.
	stepsTable = "triage_steps"
)

// schema is applied to a new database. It is idempotent so that a restart reopens the same file.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS ` + snapshotsTable + ` (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		triage_uid TEXT NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		phase TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		status TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ` + snapshotsTable + `_by_triage ON ` + snapshotsTable + ` (namespace, name, id)`,
	`CREATE TRIGGER IF NOT EXISTS ` + snapshotsTable + `_no_update
		BEFORE UPDATE ON ` + snapshotsTable + `
		BEGIN SELECT RAISE(ABORT, '` + snapshotsTable + ` is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS ` + snapshotsTable + `_no_delete
		BEFORE DELETE ON ` + snapshotsTable + `
		BEGIN SELECT RAISE(ABORT, '` + snapshotsTable + ` is append-only'); END`,
	`CREATE TABLE IF NOT EXISTS ` + stepsTable + ` (
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
	`CREATE INDEX IF NOT EXISTS ` + stepsTable + `_by_triage ON ` + stepsTable + ` (namespace, name, id)`,
	`CREATE TRIGGER IF NOT EXISTS ` + stepsTable + `_no_update
		BEFORE UPDATE ON ` + stepsTable + `
		BEGIN SELECT RAISE(ABORT, '` + stepsTable + ` is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS ` + stepsTable + `_no_delete
		BEFORE DELETE ON ` + stepsTable + `
		BEGIN SELECT RAISE(ABORT, '` + stepsTable + ` is append-only'); END`,
}

// Store is an append-only history database.
type Store struct {
	db *sql.DB
}

// Snapshot is one recorded Triage status.
type Snapshot struct {
	// Triage is the name of the Triage.
	Triage string
	// UID is the UID of the Triage, so a recreated Triage with the same name stays distinguishable.
	UID string
	// Namespace is the namespace of the Triage.
	Namespace string
	// Phase is the phase recorded in the snapshot.
	Phase v1alpha1.Phase
	// ObservedAt is when the server wrote the status.
	ObservedAt time.Time
	// Status is the complete Triage status at that moment.
	Status v1alpha1.TriageStatus
}

// Step is one recorded step event.
type Step struct {
	// Triage is the name of the Triage.
	Triage string
	// UID is the UID of the Triage.
	UID string
	// Namespace is the namespace of the Triage.
	Namespace string
	// RecordedAt is when the server accepted the step.
	RecordedAt time.Time
	// Kind classifies the step.
	Kind v1alpha1.StepKind
	// Tool names the tool for a ToolCall step.
	Tool string
	// Summary is the bounded summary the server kept.
	Summary string
	// Finding is the Finding the step carried, when it carried one.
	Finding *v1alpha1.Finding
}

// Open opens the history database at path, creating the schema when the file is new. It fails when the file was
// written by a newer schema than this package understands.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("a history path is required")
	}
	store, err := open(path, true)
	if err != nil {
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// OpenReader opens an existing history database for reading. It never creates or migrates the file, so a reader
// cannot turn a missing database into an empty history.
func OpenReader(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("a history path is required")
	}
	return open(path, false)
}

// open opens the file with the pragmas that make one writer and many readers safe. WAL keeps a reader from
// blocking the server's appends, and busy_timeout lets a writer wait out a short reader transaction. Only the
// writer switches the journal mode, because that pragma writes to the file.
func open(path string, writer bool) (*Store, error) {
	location := url.URL{Scheme: "file", Path: path}
	dsn := location.String() + "?_pragma=busy_timeout(5000)"
	if writer {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open the history database: %w", err)
	}
	// One connection serializes writes inside this process; readers open their own Store.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to the history database: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate creates the schema and records its version.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read the history schema version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("the history schema is version %d; this build reads version %d", version, SchemaVersion)
	}
	for _, statement := range schema {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create the history schema: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("record the history schema version: %w", err)
	}
	return nil
}

// AppendSnapshot records a Triage status at the moment the server wrote it. It appends; it never replaces an
// earlier snapshot.
func (s *Store) AppendSnapshot(ctx context.Context, tr *v1alpha1.Triage, at time.Time) error {
	status, err := json.Marshal(tr.Status)
	if err != nil {
		return fmt.Errorf("encode the status of Triage %s: %w", tr.Name, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO `+snapshotsTable+` (triage_uid, namespace, name, phase, observed_at, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(tr.UID), tr.Namespace, tr.Name, string(tr.Status.Phase), formatTime(at), string(status),
	)
	if err != nil {
		return fmt.Errorf("append a snapshot of Triage %s: %w", tr.Name, err)
	}
	return nil
}

// AppendStep records one step event. The Finding is stored as JSON when the step carried one.
func (s *Store) AppendStep(
	ctx context.Context, tr *v1alpha1.Triage, step v1alpha1.Step, finding *v1alpha1.Finding,
) error {
	var encoded any
	if finding != nil {
		raw, err := json.Marshal(finding)
		if err != nil {
			return fmt.Errorf("encode a finding of Triage %s: %w", tr.Name, err)
		}
		encoded = string(raw)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO `+stepsTable+` (triage_uid, namespace, name, recorded_at, kind, tool, summary, finding)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(tr.UID), tr.Namespace, tr.Name, formatTime(step.Time.Time),
		string(step.Kind), step.Tool, step.Summary, encoded,
	)
	if err != nil {
		return fmt.Errorf("append a step of Triage %s: %w", tr.Name, err)
	}
	return nil
}

// Snapshots returns the snapshots of a Triage in the order they were appended.
func (s *Store) Snapshots(ctx context.Context, triage string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT triage_uid, namespace, name, phase, observed_at, status
		 FROM `+snapshotsTable+` WHERE name = ? ORDER BY id`, triage)
	if err != nil {
		return nil, fmt.Errorf("read the snapshots of Triage %s: %w", triage, err)
	}
	defer func() { _ = rows.Close() }()
	var snapshots []Snapshot
	for rows.Next() {
		var (
			snapshot  Snapshot
			phase     string
			observed  string
			statusRaw string
		)
		if err := rows.Scan(&snapshot.UID, &snapshot.Namespace, &snapshot.Triage, &phase, &observed, &statusRaw); err != nil {
			return nil, fmt.Errorf("read a snapshot of Triage %s: %w", triage, err)
		}
		snapshot.Phase = v1alpha1.Phase(phase)
		if snapshot.ObservedAt, err = parseTime(observed); err != nil {
			return nil, fmt.Errorf("read the time of a snapshot of Triage %s: %w", triage, err)
		}
		if err := json.Unmarshal([]byte(statusRaw), &snapshot.Status); err != nil {
			return nil, fmt.Errorf("decode a snapshot of Triage %s: %w", triage, err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the snapshots of Triage %s: %w", triage, err)
	}
	return snapshots, nil
}

// Steps returns the step events of a Triage in the order they were appended.
func (s *Store) Steps(ctx context.Context, triage string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT triage_uid, namespace, name, recorded_at, kind, tool, summary, finding
		 FROM `+stepsTable+` WHERE name = ? ORDER BY id`, triage)
	if err != nil {
		return nil, fmt.Errorf("read the steps of Triage %s: %w", triage, err)
	}
	defer func() { _ = rows.Close() }()
	var steps []Step
	for rows.Next() {
		var (
			step     Step
			recorded string
			kind     string
			finding  sql.NullString
		)
		if err := rows.Scan(
			&step.UID, &step.Namespace, &step.Triage, &recorded, &kind, &step.Tool, &step.Summary, &finding,
		); err != nil {
			return nil, fmt.Errorf("read a step of Triage %s: %w", triage, err)
		}
		step.Kind = v1alpha1.StepKind(kind)
		if step.RecordedAt, err = parseTime(recorded); err != nil {
			return nil, fmt.Errorf("read the time of a step of Triage %s: %w", triage, err)
		}
		if finding.Valid {
			var recordedFinding v1alpha1.Finding
			if err := json.Unmarshal([]byte(finding.String), &recordedFinding); err != nil {
				return nil, fmt.Errorf("decode a finding of Triage %s: %w", triage, err)
			}
			step.Finding = &recordedFinding
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the steps of Triage %s: %w", triage, err)
	}
	return steps, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close the history database: %w", err)
	}
	return nil
}

// formatTime writes a time as UTC with nanosecond precision.
func formatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// parseTime reads a time written by formatTime.
func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

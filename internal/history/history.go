// Package history is the append-only SQLite record of what Pagerless observed. It is written by
// pagerless-server on every Phase change and for every step event, and is never read to decide anything about an
// Active Page: the Page resource remains the source of truth (ADR-0001).
//
// Schema version 2 has two tables, each with a trigger that refuses UPDATE and a trigger that refuses DELETE:
//
//	page_snapshots(id, page_uid, namespace, name, phase, observed_at, created_at, spec, status)
//	page_steps(id, page_uid, namespace, name, recorded_at, kind, tool, summary, finding)
//
// A snapshot stores the whole Page status and spec as JSON next to the phase, the time the Page was created,
// and the time the status was observed. The spec is recorded so the dashboard can still show the Target and the
// Trigger after retention has deleted the Page resource. A step stores the bounded step the server accepted,
// with its Finding as JSON when the step carried one. History therefore cannot be rewritten by the application.
//
// Schema version 3 adds the two mutable runtime tables that deduplication needs, because a Cooldown must
// outlive the Page resource and the one-hour Event TTL:
//
//	failure_cooldowns(fingerprint, cluster_id, subject_key, …, next_backoff_level, cooldown_until, …)
//	failure_observations(fingerprint, subject_key, state, severity, …, last_count, last_trigger_at, …)
//
// They are authoritative runtime state, not history: they are updated in place, they are written only by
// pagerless-server, and internal/cooldown owns their meaning. They live in this file because the Cooldown, the
// suppression counter, and the settle state must survive a restart, an expired Event, and a deleted Page, and
// because one SQLite file with one write-ahead-log setting has one writer. The append-only invariant of the two
// history tables is unchanged.
//
// Version 2 added the created_at and spec columns; a version 1 database is upgraded in place. Version 3 adds the
// runtime tables; a version 2 database gains them on first start without rewriting a row.
//
// Schema version 4 adds one table, and only adds it:
//
//	proposal_contents(proposal_hash, path, content, recorded_at)
//
// It holds the complete content of a Proposal's files, keyed by the Proposal hash, because the Page records only
// each file's digest and rationale. Every statement is idempotent, so a version 1, 2, or 3 database gains the new
// tables in place without rewriting a row.
package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	// The pure-Go SQLite driver registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// SchemaVersion is the version of the schema that this package creates and reads.
const SchemaVersion = 4

const (
	// snapshotsTable holds one row per Phase change.
	snapshotsTable = "page_snapshots"
	// stepsTable holds one row per step event.
	stepsTable = "page_steps"
	// contentsTable holds the complete content of a Proposal's files.
	contentsTable = "proposal_contents"
)

// schema is applied to a new database. It is idempotent so that a restart reopens the same file.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS ` + snapshotsTable + ` (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		page_uid TEXT NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		phase TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		created_at TEXT,
		spec TEXT,
		status TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ` + snapshotsTable + `_by_page ON ` + snapshotsTable + ` (namespace, name, id)`,
	`CREATE TRIGGER IF NOT EXISTS ` + snapshotsTable + `_no_update
		BEFORE UPDATE ON ` + snapshotsTable + `
		BEGIN SELECT RAISE(ABORT, '` + snapshotsTable + ` is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS ` + snapshotsTable + `_no_delete
		BEFORE DELETE ON ` + snapshotsTable + `
		BEGIN SELECT RAISE(ABORT, '` + snapshotsTable + ` is append-only'); END`,
	`CREATE TABLE IF NOT EXISTS ` + stepsTable + ` (
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
	`CREATE INDEX IF NOT EXISTS ` + stepsTable + `_by_page ON ` + stepsTable + ` (namespace, name, id)`,
	`CREATE TRIGGER IF NOT EXISTS ` + stepsTable + `_no_update
		BEFORE UPDATE ON ` + stepsTable + `
		BEGIN SELECT RAISE(ABORT, '` + stepsTable + ` is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS ` + stepsTable + `_no_delete
		BEFORE DELETE ON ` + stepsTable + `
		BEGIN SELECT RAISE(ABORT, '` + stepsTable + ` is append-only'); END`,
	// Version 3. One row per failure Fingerprint; authoritative for the Cooldown and
	// the suppression counter. Every statement is idempotent, so a sibling change
	// that adds a table to the same file cannot collide with this one.
	`CREATE TABLE IF NOT EXISTS failure_cooldowns (
		fingerprint TEXT PRIMARY KEY,
		cluster_id TEXT NOT NULL,
		subject_key TEXT NOT NULL,
		api_version TEXT NOT NULL,
		kind TEXT NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		uid TEXT NOT NULL,
		reason_class TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT '',
		next_backoff_level INTEGER NOT NULL DEFAULT 0,
		last_paged_at TEXT NOT NULL,
		cooldown_until TEXT NOT NULL,
		suppressed_count INTEGER NOT NULL DEFAULT 0,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		last_resolved_at TEXT,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS failure_cooldowns_by_subject
		ON failure_cooldowns (cluster_id, subject_key, last_seen_at DESC)`,
	`CREATE INDEX IF NOT EXISTS failure_cooldowns_by_cooldown ON failure_cooldowns (cooldown_until)`,
	`CREATE INDEX IF NOT EXISTS failure_cooldowns_by_paged_at ON failure_cooldowns (last_paged_at)`,
	// Version 3. Settling state: a row exists while a signal is observed, paged, or clearing.
	`CREATE TABLE IF NOT EXISTS failure_observations (
		fingerprint TEXT PRIMARY KEY,
		subject_key TEXT NOT NULL,
		state TEXT NOT NULL,
		severity TEXT NOT NULL,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		consecutive INTEGER NOT NULL DEFAULT 0,
		occurrences INTEGER NOT NULL DEFAULT 0,
		last_count INTEGER NOT NULL DEFAULT 0,
		last_note TEXT NOT NULL DEFAULT '',
		last_trigger_at TEXT,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS failure_observations_by_subject
		ON failure_observations (subject_key, last_seen_at DESC)`,
	// Version 4. The complete content of a Proposal's files, keyed by the Proposal hash.
	`CREATE TABLE IF NOT EXISTS ` + contentsTable + ` (
		proposal_hash TEXT NOT NULL,
		path TEXT NOT NULL,
		content BLOB NOT NULL,
		recorded_at TEXT NOT NULL,
		PRIMARY KEY (proposal_hash, path)
	)`,
	`CREATE TRIGGER IF NOT EXISTS ` + contentsTable + `_no_update
		BEFORE UPDATE ON ` + contentsTable + `
		BEGIN SELECT RAISE(ABORT, '` + contentsTable + ` is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS ` + contentsTable + `_no_delete
		BEFORE DELETE ON ` + contentsTable + `
		BEGIN SELECT RAISE(ABORT, '` + contentsTable + ` is append-only'); END`,
}

// Store is an append-only history database.
type Store struct {
	db *sql.DB
}

// Snapshot is one recorded Page status.
type Snapshot struct {
	// Page is the name of the Page.
	Page string
	// UID is the UID of the Page, so a recreated Page with the same name stays distinguishable.
	UID string
	// Namespace is the namespace of the Page.
	Namespace string
	// Phase is the phase recorded in the snapshot.
	Phase v1alpha1.Phase
	// ObservedAt is when the server wrote the status.
	ObservedAt time.Time
	// CreatedAt is when the Page was created. It is zero for a snapshot written by schema version 1.
	CreatedAt time.Time
	// Spec is the Page's immutable spec, recorded so the dashboard can show the Target and the Trigger after
	// retention has deleted the Page resource. It is zero for a snapshot written by schema version 1.
	Spec v1alpha1.PageSpec
	// Status is the complete Page status at that moment.
	Status v1alpha1.PageStatus
}

// Step is one recorded step event.
type Step struct {
	// Page is the name of the Page.
	Page string
	// UID is the UID of the Page.
	UID string
	// Namespace is the namespace of the Page.
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

// dataSourceName builds the SQLite URI for path. A Windows drive path is not a
// URI path, so it needs a leading slash: without one the drive letter parses as
// a URI authority and the driver rejects the DSN.
func dataSourceName(path string, writer bool) string {
	location := url.URL{Scheme: "file", Path: uriPath(path)}
	dsn := location.String() + "?_pragma=busy_timeout(5000)"
	if writer {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	return dsn
}

func uriPath(path string) string {
	// A Windows drive path reaches this process on any platform, from a test or
	// a caller that names a Windows share, so the separators are normalised
	// unconditionally; filepath.ToSlash would leave them on a POSIX build and
	// the URL would escape each backslash as %5C.
	slashed := strings.ReplaceAll(path, `\`, "/")
	if !strings.HasPrefix(slashed, "/") {
		return "/" + slashed
	}
	return slashed
}

// open opens the file with the pragmas that make one writer and many readers safe. WAL keeps a reader from
// blocking the server's appends, and busy_timeout lets a writer wait out a short reader transaction. Only the
// writer switches the journal mode, because that pragma writes to the file.
func open(path string, writer bool) (*Store, error) {
	dsn := dataSourceName(path, writer)
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
	// A database written by schema version 1 has the tables but not the columns that version 2 added.
	for _, column := range []struct{ name, definition string }{
		{"created_at", "TEXT"},
		{"spec", "TEXT"},
	} {
		if err := s.ensureColumn(ctx, snapshotsTable, column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("record the history schema version: %w", err)
	}
	return nil
}

// ensureColumn adds a nullable column to a table that does not have it yet, so that an older database is upgraded
// without rewriting any row.
func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	found, err := s.hasColumn(ctx, table, column)
	if err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		return fmt.Errorf("add the %s column to %s: %w", column, table, err)
	}
	return nil
}

// hasColumn reports whether the table already has the column.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			position   int
			name       string
			columnType string
			notNull    int
			fallback   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&position, &name, &columnType, &notNull, &fallback, &primaryKey); err != nil {
			return false, fmt.Errorf("read a column of %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	return false, nil
}

// AppendSnapshot records a Page status at the moment the server wrote it. It appends; it never replaces an
// earlier snapshot.
func (s *Store) AppendSnapshot(ctx context.Context, tr *v1alpha1.Page, at time.Time) error {
	status, err := json.Marshal(tr.Status)
	if err != nil {
		return fmt.Errorf("encode the status of Page %s: %w", tr.Name, err)
	}
	spec, err := json.Marshal(tr.Spec)
	if err != nil {
		return fmt.Errorf("encode the spec of Page %s: %w", tr.Name, err)
	}
	var createdAt any
	if !tr.CreationTimestamp.IsZero() {
		createdAt = formatTime(tr.CreationTimestamp.Time)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO `+snapshotsTable+` (page_uid, namespace, name, phase, observed_at, created_at, spec, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(tr.UID), tr.Namespace, tr.Name, string(tr.Status.Phase), formatTime(at), createdAt,
		string(spec), string(status),
	)
	if err != nil {
		return fmt.Errorf("append a snapshot of Page %s: %w", tr.Name, err)
	}
	return nil
}

// AppendStep records one step event. The Finding is stored as JSON when the step carried one.
func (s *Store) AppendStep(
	ctx context.Context, tr *v1alpha1.Page, step v1alpha1.Step, finding *v1alpha1.Finding,
) error {
	var encoded any
	if finding != nil {
		raw, err := json.Marshal(finding)
		if err != nil {
			return fmt.Errorf("encode a finding of Page %s: %w", tr.Name, err)
		}
		encoded = string(raw)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO `+stepsTable+` (page_uid, namespace, name, recorded_at, kind, tool, summary, finding)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(tr.UID), tr.Namespace, tr.Name, formatTime(step.Time.Time),
		string(step.Kind), step.Tool, step.Summary, encoded,
	)
	if err != nil {
		return fmt.Errorf("append a step of Page %s: %w", tr.Name, err)
	}
	return nil
}

// Snapshots returns the snapshots of a Page in the order they were appended.
func (s *Store) Snapshots(ctx context.Context, page string) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT page_uid, namespace, name, phase, observed_at, created_at, spec, status
		 FROM `+snapshotsTable+` WHERE name = ? ORDER BY id`, page)
	if err != nil {
		return nil, fmt.Errorf("read the snapshots of Page %s: %w", page, err)
	}
	defer func() { _ = rows.Close() }()
	var snapshots []Snapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("read a snapshot of Page %s: %w", page, err)
		}
		snapshots = append(snapshots, *snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the snapshots of Page %s: %w", page, err)
	}
	return snapshots, nil
}

// Latest returns the most recent snapshot of every Page, most recently appended first. It is what the
// dashboard's list starts from, so a Page whose resource retention deleted still appears.
func (s *Store) Latest(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.page_uid, s.namespace, s.name, s.phase, s.observed_at, s.created_at, s.spec, s.status
		 FROM `+snapshotsTable+` s
		 JOIN (SELECT namespace, name, MAX(id) AS id FROM `+snapshotsTable+` GROUP BY namespace, name) latest
		   ON s.id = latest.id
		 ORDER BY s.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("read the latest snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var snapshots []Snapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("read a latest snapshot: %w", err)
		}
		snapshots = append(snapshots, *snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the latest snapshots: %w", err)
	}
	return snapshots, nil
}

// scanner is the subset of *sql.Rows that scanSnapshot reads.
type scanner interface {
	Scan(dest ...any) error
}

// scanSnapshot reads one snapshot row in the column order that both queries select.
func scanSnapshot(row scanner) (*Snapshot, error) {
	var (
		snapshot  Snapshot
		phase     string
		observed  string
		created   sql.NullString
		specRaw   sql.NullString
		statusRaw string
	)
	if err := row.Scan(
		&snapshot.UID, &snapshot.Namespace, &snapshot.Page, &phase, &observed, &created, &specRaw, &statusRaw,
	); err != nil {
		return nil, err
	}
	snapshot.Phase = v1alpha1.Phase(phase)
	var err error
	if snapshot.ObservedAt, err = parseTime(observed); err != nil {
		return nil, err
	}
	if created.Valid {
		if snapshot.CreatedAt, err = parseTime(created.String); err != nil {
			return nil, err
		}
	}
	if specRaw.Valid && specRaw.String != "" {
		if err := json.Unmarshal([]byte(specRaw.String), &snapshot.Spec); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal([]byte(statusRaw), &snapshot.Status); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// Steps returns the step events of a Page in the order they were appended.
func (s *Store) Steps(ctx context.Context, page string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT page_uid, namespace, name, recorded_at, kind, tool, summary, finding
		 FROM `+stepsTable+` WHERE name = ? ORDER BY id`, page)
	if err != nil {
		return nil, fmt.Errorf("read the steps of Page %s: %w", page, err)
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
			&step.UID, &step.Namespace, &step.Page, &recorded, &kind, &step.Tool, &step.Summary, &finding,
		); err != nil {
			return nil, fmt.Errorf("read a step of Page %s: %w", page, err)
		}
		step.Kind = v1alpha1.StepKind(kind)
		if step.RecordedAt, err = parseTime(recorded); err != nil {
			return nil, fmt.Errorf("read the time of a step of Page %s: %w", page, err)
		}
		if finding.Valid {
			var recordedFinding v1alpha1.Finding
			if err := json.Unmarshal([]byte(finding.String), &recordedFinding); err != nil {
				return nil, fmt.Errorf("decode a finding of Page %s: %w", page, err)
			}
			step.Finding = &recordedFinding
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the steps of Page %s: %w", page, err)
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

// DB returns the connection this Store writes through, so that runtime-state
// packages (internal/cooldown) share the history file's write-ahead-log and
// busy-timeout settings and its single writer rather than opening a second
// connection with its own pragmas. The Store owns the handle: a caller must not
// close it.
func (s *Store) DB() *sql.DB {
	return s.db
}

// formatTime writes a time as UTC with nanosecond precision.
func formatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// parseTime reads a time written by formatTime.
func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

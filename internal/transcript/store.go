// Package transcript is the durable, ordered record of investigations. It
// stores pages, turns, messages, append-only events, sanitized tool evidence
// and published proposals in one SQLite database.
//
// Every page numbers its events from one. A write that appends an event takes
// the page's next number inside the same transaction that stores the change, so
// two writers can never produce the same sequence number and a reader that
// resumes after a sequence number sees every later event exactly once.
package transcript

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go SQLite driver
)

// SchemaVersion is the number of the current schema. A database with a newer
// number is refused rather than opened, because this binary cannot know what an
// unknown migration means.
const SchemaVersion = 1

// Page and turn statuses. Pages carry the status of their most recent turn.
const (
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusStopped     = "stopped"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Event types.
const (
	EventTurnStarted  = "turn.started"
	EventToolStarted  = "tool.started"
	EventToolFinished = "tool.finished"
	EventAnswer       = "answer"
	EventProposal     = "proposal"
	EventTurnFinished = "turn.finished"
)

// Evidence statuses. They are the status values the private callback returns.
const (
	EvidenceOK              = "ok"
	EvidenceDenied          = "denied"
	EvidenceFailed          = "failed"
	EvidenceTimedOut        = "timed_out"
	EvidenceTruncated       = "truncated"
	EvidenceBudgetExhausted = "budget_exhausted"
)

// Errors returned by the store and rendered by the API.
var (
	// ErrNotFound means the page, turn, event or evidence does not exist.
	ErrNotFound = errors.New("not found")
	// ErrTurnInProgress means the page already has a turn that has not finished.
	ErrTurnInProgress = errors.New("a turn is already running")
	// ErrConflict means a write contradicts durable state.
	ErrConflict = errors.New("conflict")
)

// Options configures how the store opens its database file.
type Options struct {
	// Path is the database file. The directory must exist.
	Path string
	// BusyTimeout bounds how long one writer waits for another process.
	BusyTimeout time.Duration
}

// Store is a SQLite-backed transcript. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// writeMu serializes writers within this process. SQLite allows one writer
	// anyway; holding the lock here makes the ordering explicit and keeps the
	// busy handler out of the common path.
	writeMu sync.Mutex
	// notify coalesces event notifications per page.
	notifyMu sync.Mutex
	notify   map[string]chan struct{}
}

// Open opens or creates the database and applies the current schema.
func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("the transcript needs a database path")
	}
	timeout := options.BusyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dsn := "file:" + url.PathEscape(options.Path) +
		"?_pragma=busy_timeout(" + fmt.Sprint(timeout.Milliseconds()) + ")" +
		"&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open the transcript: %w", err)
	}
	// One writer and one reader keep SQLite contention predictable. The pool is
	// small because the service is a single replica by design.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db, notify: make(map[string]chan struct{})}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database handles.
func (s *Store) Close() error { return s.db.Close() }

// Path joins a directory and the transcript file name.
func Path(directory string) string { return filepath.Join(directory, "pagerless.db") }

func (s *Store) migrate(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var version int
	err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	if err != nil {
		return fmt.Errorf("read the transcript schema version: %w", err)
	}
	switch {
	case version == SchemaVersion:
		return nil
	case version > SchemaVersion:
		return fmt.Errorf("the transcript schema is version %d, newer than this server's %d", version, SchemaVersion)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the transcript migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range schema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply the transcript schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return fmt.Errorf("record the transcript schema version: %w", err)
	}
	return tx.Commit()
}

// schema creates a fresh database. Statements are ordered and independent.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS pages (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		status TEXT NOT NULL,
		namespace TEXT NOT NULL DEFAULT '',
		target_kind TEXT NOT NULL DEFAULT '',
		target_name TEXT NOT NULL DEFAULT '',
		target_api_version TEXT NOT NULL DEFAULT '',
		target_namespace TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		active_turn_id TEXT NOT NULL DEFAULT '',
		next_seq INTEGER NOT NULL DEFAULT 1
	)`,
	`CREATE TABLE IF NOT EXISTS turns (
		id TEXT PRIMARY KEY,
		page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		idempotency_key TEXT NOT NULL DEFAULT '',
		namespace TEXT NOT NULL DEFAULT '',
		target_kind TEXT NOT NULL DEFAULT '',
		target_name TEXT NOT NULL DEFAULT '',
		target_api_version TEXT NOT NULL DEFAULT '',
		error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS turns_by_page ON turns(page_id, created_at)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		turn_id TEXT NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		citations TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS messages_by_page ON messages(page_id, created_at)`,
	`CREATE TABLE IF NOT EXISTS events (
		page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		seq INTEGER NOT NULL,
		turn_id TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL,
		time TEXT NOT NULL,
		data TEXT NOT NULL,
		PRIMARY KEY (page_id, seq)
	)`,
	`CREATE TABLE IF NOT EXISTS evidence (
		page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		call_id TEXT NOT NULL,
		attempt INTEGER NOT NULL,
		turn_id TEXT NOT NULL,
		tool TEXT NOT NULL,
		arguments TEXT NOT NULL,
		result TEXT NOT NULL,
		status TEXT NOT NULL,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		redacted INTEGER NOT NULL DEFAULT 0,
		truncated INTEGER NOT NULL DEFAULT 0,
		bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		PRIMARY KEY (page_id, call_id, attempt)
	)`,
	`CREATE INDEX IF NOT EXISTS evidence_by_call ON evidence(page_id, call_id, attempt DESC)`,
	`CREATE TABLE IF NOT EXISTS proposals (
		page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
		turn_id TEXT NOT NULL,
		base_sha TEXT NOT NULL,
		branch TEXT NOT NULL DEFAULT '',
		pr_url TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		files TEXT NOT NULL,
		diff TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (page_id, turn_id)
	)`,
	`CREATE TABLE IF NOT EXISTS idempotency (
		key TEXT PRIMARY KEY,
		operation TEXT NOT NULL,
		page_id TEXT NOT NULL,
		turn_id TEXT NOT NULL DEFAULT '',
		request_hash TEXT NOT NULL DEFAULT '',
		response TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
}

// storedTimeLayout keeps a fixed number of fractional digits so that the
// lexicographic order of the stored text is also the chronological order.
const storedTimeLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime renders a timestamp for storage.
func formatTime(at time.Time) string { return at.UTC().Format(storedTimeLayout) }

// parseTime reads a stored timestamp. It also accepts the wire layout so a
// database written by an earlier build remains readable.
func parseTime(text string) (time.Time, error) {
	at, err := time.Parse(storedTimeLayout, text)
	if err != nil {
		at, err = time.Parse(time.RFC3339Nano, text)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("parse the stored time %q: %w", text, err)
	}
	return at.UTC(), nil
}

// notifyPage wakes every subscriber of one page, coalescing bursts: a
// subscriber re-reads the durable log after each wake, so one wake is enough.
func (s *Store) notifyPage(pageID string) {
	s.notifyMu.Lock()
	channel, ok := s.notify[pageID]
	s.notifyMu.Unlock()
	if !ok {
		return
	}
	select {
	case channel <- struct{}{}:
	default:
	}
}

// Subscribe returns a channel that receives a value after an event is appended
// to the page, and a function that releases the subscription. The channel is
// only a hint: a subscriber must read EventsAfter to see the events.
func (s *Store) Subscribe(pageID string) (<-chan struct{}, func()) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	channel, ok := s.notify[pageID]
	if !ok {
		channel = make(chan struct{}, 1)
		s.notify[pageID] = channel
	}
	return channel, func() {
		s.notifyMu.Lock()
		defer s.notifyMu.Unlock()
		if current, found := s.notify[pageID]; found && current == channel {
			delete(s.notify, pageID)
		}
	}
}

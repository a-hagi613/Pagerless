package cooldown

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"k8s.io/utils/clock"
)

// Observation states. An observation exists from the first signal until it is
// clear; only an observing, paged, or clearing observation is active.
const (
	// StateObserving means the Fingerprint is inside its settle window.
	StateObserving = "observing"
	// StatePaged means the Fingerprint opened or joined a Page.
	StatePaged = "paged"
	// StateClearing means no status evaluation re-derived the Fingerprint.
	StateClearing = "clearing"
	// StateResolved means the Fingerprint is clear.
	StateResolved = "resolved"
)

// HeartbeatInterval bounds how often an unchanged observation is written, so a
// restart storm of four hundred restarts does not amplify into four hundred
// updates. A state or severity change is always written immediately.
const HeartbeatInterval = 10 * time.Second

// maxNoteLength is the longest note an observation stores. The note is evidence
// for the model and is never part of identity.
const maxNoteLength = 1024

// ErrNotOpen is returned by BeginPage when the Fingerprint is inside its
// Cooldown and therefore must not open a Page.
var ErrNotOpen = errors.New("the fingerprint is inside its cooldown")

// Record is the durable state of one failure Fingerprint.
type Record struct {
	// Fingerprint is the domain-separated SHA-256 of the failure, lowercase hex.
	Fingerprint string
	// ClusterID identifies the Cluster that saw the failure.
	ClusterID string
	// SubjectKey is the base32 Subject identity the Fingerprint belongs to.
	SubjectKey string
	// APIVersion, Kind, Namespace, Name, and UID are the Subject's identity.
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	UID        string
	// ReasonClass is the catalog constant and Scope the container or condition.
	ReasonClass string
	Scope       string
	// NextBackoffLevel is the level the next Page will use.
	NextBackoffLevel int
	// LastPagedAt is when a Page last opened for this Fingerprint.
	LastPagedAt time.Time
	// CooldownUntil is when the Fingerprint may open a Page again.
	CooldownUntil time.Time
	// SuppressedCount counts Triggers that did not open a Page inside a Cooldown.
	SuppressedCount int64
	// FirstSeenAt and LastSeenAt bound the observations of this Fingerprint.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// LastResolvedAt is when the Subject last became healthy, or nil.
	LastResolvedAt *time.Time
	// UpdatedAt is when the row was last written.
	UpdatedAt time.Time
}

// Window is the Cooldown a Page just opened under.
type Window struct {
	// Level is the backoff level the Page used.
	Level int
	// LastPagedAt is when the Page opened.
	LastPagedAt time.Time
	// CooldownUntil is when the same Fingerprint may open a new Page.
	CooldownUntil time.Time
	// RecurrenceCount is how many Pages this Fingerprint has opened since its
	// level was last reset, this one included.
	RecurrenceCount int32
	// SuppressedCount is the number of Triggers this Fingerprint accumulated
	// inside earlier Cooldowns. The Page it opens carries it as evidence.
	SuppressedCount int64
}

// Observation is the settling state of one failure Fingerprint.
type Observation struct {
	// Fingerprint identifies the failure.
	Fingerprint string
	// SubjectKey is the Subject the Fingerprint belongs to.
	SubjectKey string
	// State is one of the State constants.
	State string
	// Severity is the severity last computed for the observation.
	Severity string
	// FirstSeenAt and LastSeenAt bound the observation.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	// Consecutive counts status evaluations that re-derived the observation.
	Consecutive int64
	// Occurrences counts the observations aggregated into the Fingerprint.
	Occurrences int64
	// LastCount is the last Event series or deprecated count that was consumed.
	LastCount int64
	// LastNote is the last note, truncated. It is evidence, never identity.
	LastNote string
	// LastTriggerAt is when this Fingerprint last produced a Trigger, which
	// bounds the aggregation window.
	LastTriggerAt time.Time
	// UpdatedAt is when the row was last written.
	UpdatedAt time.Time
}

// Store reads and writes the runtime tables in the history SQLite file.
type Store struct {
	db     *sql.DB
	ladder Ladder
	clock  clock.PassiveClock
}

// Option configures a Store.
type Option func(*Store)

// WithLadder replaces the Cooldown ladder.
func WithLadder(l Ladder) Option {
	return func(s *Store) { s.ladder = l }
}

// WithClock replaces the clock the Store stamps rows with.
func WithClock(c clock.PassiveClock) Option {
	return func(s *Store) { s.clock = c }
}

// New builds a Store over the handle history opened. It validates the ladder so
// a value that cannot bound a flap fails at startup.
func New(db *sql.DB, opts ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("a database handle is required")
	}
	s := &Store{db: db, ladder: DefaultLadder(), clock: clock.RealClock{}}
	for _, opt := range opts {
		opt(s)
	}
	if s.ladder.ResetAfterHealthy <= 0 {
		s.ladder.ResetAfterHealthy = DefaultResetAfterHealthy
	}
	if err := s.ladder.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Ladder returns the ladder the Store applies.
func (s *Store) Ladder() Ladder { return s.ladder }

// now returns the Store's clock time.
func (s *Store) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}

// Get returns the Record for fingerprint, and whether it exists.
func (s *Store) Get(ctx context.Context, fingerprint string) (Record, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT fingerprint, cluster_id, subject_key, api_version, kind, namespace, name, uid,
		        reason_class, scope, next_backoff_level, last_paged_at, cooldown_until,
		        suppressed_count, first_seen_at, last_seen_at, last_resolved_at, updated_at
		 FROM failure_cooldowns WHERE fingerprint = ?`, fingerprint)
	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read the cooldown of fingerprint %s: %w", fingerprint, err)
	}
	return *record, true, nil
}

// BeginPage applies the Cooldown ladder for a Page that is about to open. It
// advances the ladder and records the window in one transaction, because a
// crash after the write but before the Page exists must suppress the next
// Trigger rather than open a duplicate Page. It returns ErrNotOpen, and counts
// the suppression, when the Fingerprint is inside its Cooldown.
func (s *Store) BeginPage(ctx context.Context, rec Record, at time.Time) (Window, error) {
	at = at.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Window{}, fmt.Errorf("begin the cooldown transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanRecord(tx.QueryRowContext(ctx,
		`SELECT fingerprint, cluster_id, subject_key, api_version, kind, namespace, name, uid,
		        reason_class, scope, next_backoff_level, last_paged_at, cooldown_until,
		        suppressed_count, first_seen_at, last_seen_at, last_resolved_at, updated_at
		 FROM failure_cooldowns WHERE fingerprint = ?`, rec.Fingerprint))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Window{}, fmt.Errorf("read the cooldown of fingerprint %s: %w", rec.Fingerprint, err)
	}
	if found && at.Before(existing.CooldownUntil) {
		if err := suppress(ctx, tx, rec.Fingerprint, at); err != nil {
			return Window{}, err
		}
		if err := tx.Commit(); err != nil {
			return Window{}, fmt.Errorf("commit a suppression: %w", err)
		}
		return Window{}, ErrNotOpen
	}

	level := 0
	firstSeen := rec.FirstSeenAt
	suppressed := int64(0)
	if found {
		level = existing.NextBackoffLevel
		firstSeen = existing.FirstSeenAt
		suppressed = existing.SuppressedCount
	}
	window := Window{
		Level:           level,
		LastPagedAt:     at,
		CooldownUntil:   s.ladder.CooldownUntil(level, rec.Fingerprint, at),
		RecurrenceCount: int32(level + 1),
		SuppressedCount: suppressed,
	}
	next := s.ladder.Next(level + 1)
	if err := writeRecord(ctx, tx, Record{
		Fingerprint:      rec.Fingerprint,
		ClusterID:        rec.ClusterID,
		SubjectKey:       rec.SubjectKey,
		APIVersion:       rec.APIVersion,
		Kind:             rec.Kind,
		Namespace:        rec.Namespace,
		Name:             rec.Name,
		UID:              rec.UID,
		ReasonClass:      rec.ReasonClass,
		Scope:            rec.Scope,
		NextBackoffLevel: next,
		LastPagedAt:      at,
		CooldownUntil:    window.CooldownUntil,
		SuppressedCount:  suppressed,
		FirstSeenAt:      firstSeen,
		LastSeenAt:       at,
		UpdatedAt:        at,
	}); err != nil {
		return Window{}, err
	}
	if err := tx.Commit(); err != nil {
		return Window{}, fmt.Errorf("commit a cooldown: %w", err)
	}
	return window, nil
}

// Suppress counts one Trigger that arrived inside a Cooldown without opening a
// Page and returns the new suppressed count.
func (s *Store) Suppress(ctx context.Context, fingerprint string, at time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin the suppression transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := suppress(ctx, tx, fingerprint, at.UTC()); err != nil {
		return 0, err
	}
	var count int64
	if err := tx.QueryRowContext(ctx,
		`SELECT suppressed_count FROM failure_cooldowns WHERE fingerprint = ?`, fingerprint,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("read the suppressed count of fingerprint %s: %w", fingerprint, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit a suppression: %w", err)
	}
	return count, nil
}

// suppress increments the suppressed count and the last-seen time of a row.
func suppress(ctx context.Context, tx *sql.Tx, fingerprint string, at time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE failure_cooldowns
		 SET suppressed_count = suppressed_count + 1, last_seen_at = ?, updated_at = ?
		 WHERE fingerprint = ?`, formatTime(at), formatTime(at), fingerprint)
	if err != nil {
		return fmt.Errorf("record a suppression of fingerprint %s: %w", fingerprint, err)
	}
	return nil
}

// Seed inserts a Record that was reconstructed from a Page's status when SQLite
// has no row. It never replaces an existing row, so a restored Page can only
// add state, never reset it.
func (s *Store) Seed(ctx context.Context, rec Record) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO failure_cooldowns (fingerprint, cluster_id, subject_key, api_version, kind, namespace,
		                                name, uid, reason_class, scope, next_backoff_level, last_paged_at,
		                                cooldown_until, suppressed_count, first_seen_at, last_seen_at,
		                                last_resolved_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
		 ON CONFLICT (fingerprint) DO NOTHING`,
		rec.Fingerprint, rec.ClusterID, rec.SubjectKey, rec.APIVersion, rec.Kind, rec.Namespace,
		rec.Name, rec.UID, rec.ReasonClass, rec.Scope, rec.NextBackoffLevel, formatTime(rec.LastPagedAt),
		formatTime(rec.CooldownUntil), rec.SuppressedCount, formatTime(rec.FirstSeenAt),
		formatTime(rec.LastSeenAt), formatTime(rec.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("seed the cooldown of fingerprint %s: %w", rec.Fingerprint, err)
	}
	return nil
}

// MarkResolved records that the Subject behind subjectKey is healthy, which
// starts the reset clock for every Fingerprint it carried.
func (s *Store) MarkResolved(ctx context.Context, subjectKey string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE failure_cooldowns SET last_resolved_at = ?, updated_at = ? WHERE subject_key = ?`,
		formatTime(at), formatTime(at), subjectKey)
	if err != nil {
		return fmt.Errorf("mark subject %s resolved: %w", subjectKey, err)
	}
	return nil
}

// Reset returns every Fingerprint whose Subject has been healthy for the reset
// window and has no active observation to its first level. A Page that
// "recovered" in status but flaps two minutes later has not been healthy for
// that window, so it keeps its level.
func (s *Store) Reset(ctx context.Context, at time.Time) (int64, error) {
	cutoff := at.UTC().Add(-s.ladder.ResetAfterHealthy)
	result, err := s.db.ExecContext(ctx,
		`UPDATE failure_cooldowns
		 SET next_backoff_level = 0, suppressed_count = 0, cooldown_until = last_resolved_at, updated_at = ?
		 WHERE last_resolved_at IS NOT NULL
		   AND last_resolved_at <= ?
		   AND (next_backoff_level > 0 OR suppressed_count > 0)
		   AND subject_key NOT IN (
		       SELECT subject_key FROM failure_observations WHERE state IN (?, ?, ?)
		   )`,
		formatTime(at.UTC()), formatTime(cutoff), StateObserving, StatePaged, StateClearing,
	)
	if err != nil {
		return 0, fmt.Errorf("reset healthy cooldowns: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count reset cooldowns: %w", err)
	}
	return rows, nil
}

// PagedSince counts the Pages that started at or after since. It backs the
// sliding hourly ceiling, which bounds investigations rather than Page objects.
func (s *Store) PagedSince(ctx context.Context, since time.Time) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM failure_cooldowns WHERE last_paged_at >= ?`,
		formatTime(since),
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count Pages since %s: %w", formatTime(since), err)
	}
	return count, nil
}

// LastPage returns the most recent Record for a Subject, which is how a
// Recurrence finds the Page it links to.
func (s *Store) LastPage(ctx context.Context, clusterID, subjectKey string) (Record, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT fingerprint, cluster_id, subject_key, api_version, kind, namespace, name, uid,
		        reason_class, scope, next_backoff_level, last_paged_at, cooldown_until,
		        suppressed_count, first_seen_at, last_seen_at, last_resolved_at, updated_at
		 FROM failure_cooldowns WHERE cluster_id = ? AND subject_key = ?
		 ORDER BY last_paged_at DESC LIMIT 1`, clusterID, subjectKey)
	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read the last cooldown of subject %s: %w", subjectKey, err)
	}
	return *record, true, nil
}

// PutObservation stores an observation. A state or severity change is written
// immediately; an unchanged heartbeat is written at most once per
// HeartbeatInterval. It reports whether it wrote.
func (s *Store) PutObservation(ctx context.Context, ob Observation, at time.Time) (bool, error) {
	at = at.UTC()
	ob.LastNote = truncate(ob.LastNote)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin the observation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanObservation(tx.QueryRowContext(ctx,
		`SELECT fingerprint, subject_key, state, severity, first_seen_at, last_seen_at, consecutive,
		        occurrences, last_count, last_note, last_trigger_at, updated_at
		 FROM failure_observations WHERE fingerprint = ?`, ob.Fingerprint))
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read the observation of fingerprint %s: %w", ob.Fingerprint, err)
	}
	transition := !found || existing.State != ob.State || existing.Severity != ob.Severity
	if !transition && at.Sub(existing.UpdatedAt) < HeartbeatInterval {
		return false, nil
	}
	lastTrigger := any(nil)
	if !ob.LastTriggerAt.IsZero() {
		lastTrigger = formatTime(ob.LastTriggerAt)
	}
	if found && existing.LastTriggerAt.After(ob.LastTriggerAt) {
		lastTrigger = formatTime(existing.LastTriggerAt)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO failure_observations (fingerprint, subject_key, state, severity, first_seen_at,
		                                   last_seen_at, consecutive, occurrences, last_count, last_note,
		                                   last_trigger_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (fingerprint) DO UPDATE SET
		     subject_key = excluded.subject_key,
		     state = excluded.state,
		     severity = excluded.severity,
		     first_seen_at = excluded.first_seen_at,
		     last_seen_at = excluded.last_seen_at,
		     consecutive = excluded.consecutive,
		     occurrences = excluded.occurrences,
		     last_count = excluded.last_count,
		     last_note = excluded.last_note,
		     last_trigger_at = excluded.last_trigger_at,
		     updated_at = excluded.updated_at`,
		ob.Fingerprint, ob.SubjectKey, ob.State, ob.Severity, formatTime(ob.FirstSeenAt),
		formatTime(ob.LastSeenAt), ob.Consecutive, ob.Occurrences, ob.LastCount, ob.LastNote,
		lastTrigger, formatTime(at),
	)
	if err != nil {
		return false, fmt.Errorf("store the observation of fingerprint %s: %w", ob.Fingerprint, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit an observation: %w", err)
	}
	return true, nil
}

// Observation returns one observation, and whether it exists.
func (s *Store) Observation(ctx context.Context, fingerprint string) (Observation, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT fingerprint, subject_key, state, severity, first_seen_at, last_seen_at, consecutive,
		        occurrences, last_count, last_note, last_trigger_at, updated_at
		 FROM failure_observations WHERE fingerprint = ?`, fingerprint)
	ob, err := scanObservation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, fmt.Errorf("read the observation of fingerprint %s: %w", fingerprint, err)
	}
	return *ob, true, nil
}

// SubjectObservations returns every observation of one Subject.
func (s *Store) SubjectObservations(ctx context.Context, subjectKey string) ([]Observation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, subject_key, state, severity, first_seen_at, last_seen_at, consecutive,
		        occurrences, last_count, last_note, last_trigger_at, updated_at
		 FROM failure_observations WHERE subject_key = ? ORDER BY first_seen_at`, subjectKey)
	if err != nil {
		return nil, fmt.Errorf("read the observations of subject %s: %w", subjectKey, err)
	}
	defer func() { _ = rows.Close() }()
	var observations []Observation
	for rows.Next() {
		ob, err := scanObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("read an observation of subject %s: %w", subjectKey, err)
		}
		observations = append(observations, *ob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the observations of subject %s: %w", subjectKey, err)
	}
	return observations, nil
}

// StaleObservations returns the active observations that no status evaluation
// re-derived for longer than the clear window. The watcher resolves them, which
// is how a Page clears from live status rather than from a resolution Event.
func (s *Store) StaleObservations(ctx context.Context, clearFor time.Duration, at time.Time) ([]Observation, error) {
	cutoff := at.UTC().Add(-clearFor)
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, subject_key, state, severity, first_seen_at, last_seen_at, consecutive,
		        occurrences, last_count, last_note, last_trigger_at, updated_at
		 FROM failure_observations WHERE state != ? AND last_seen_at < ? ORDER BY last_seen_at`,
		StateResolved, formatTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("read stale observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var observations []Observation
	for rows.Next() {
		ob, err := scanObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("read a stale observation: %w", err)
		}
		observations = append(observations, *ob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stale observations: %w", err)
	}
	return observations, nil
}

// ResolveObservation marks one observation resolved.
func (s *Store) ResolveObservation(ctx context.Context, fingerprint string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE failure_observations SET state = ?, updated_at = ? WHERE fingerprint = ?`,
		StateResolved, formatTime(at.UTC()), fingerprint)
	if err != nil {
		return fmt.Errorf("resolve the observation of fingerprint %s: %w", fingerprint, err)
	}
	return nil
}

// ActiveObservations reports whether the Subject still has an observation that
// is not resolved.
func (s *Store) ActiveObservations(ctx context.Context, subjectKey string) (bool, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM failure_observations WHERE subject_key = ? AND state != ?`,
		subjectKey, StateResolved,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count active observations of subject %s: %w", subjectKey, err)
	}
	return count > 0, nil
}

// rowScanner is the subset of *sql.Row and *sql.Rows that the scanners read.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner) (*Record, error) {
	var (
		rec          Record
		lastPaged    string
		cooldownTill string
		firstSeen    string
		lastSeen     string
		resolved     sql.NullString
		updated      string
	)
	if err := row.Scan(&rec.Fingerprint, &rec.ClusterID, &rec.SubjectKey, &rec.APIVersion, &rec.Kind,
		&rec.Namespace, &rec.Name, &rec.UID, &rec.ReasonClass, &rec.Scope, &rec.NextBackoffLevel,
		&lastPaged, &cooldownTill, &rec.SuppressedCount, &firstSeen, &lastSeen, &resolved, &updated); err != nil {
		return nil, err
	}
	var err error
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{
		{lastPaged, &rec.LastPagedAt},
		{cooldownTill, &rec.CooldownUntil},
		{firstSeen, &rec.FirstSeenAt},
		{lastSeen, &rec.LastSeenAt},
		{updated, &rec.UpdatedAt},
	} {
		if *field.target, err = parseTime(field.raw); err != nil {
			return nil, err
		}
	}
	if resolved.Valid {
		at, err := parseTime(resolved.String)
		if err != nil {
			return nil, err
		}
		rec.LastResolvedAt = &at
	}
	return &rec, nil
}

func scanObservation(row rowScanner) (*Observation, error) {
	var (
		ob          Observation
		firstSeen   string
		lastSeen    string
		lastTrigger sql.NullString
		updated     string
	)
	if err := row.Scan(&ob.Fingerprint, &ob.SubjectKey, &ob.State, &ob.Severity, &firstSeen, &lastSeen,
		&ob.Consecutive, &ob.Occurrences, &ob.LastCount, &ob.LastNote, &lastTrigger, &updated); err != nil {
		return nil, err
	}
	var err error
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{
		{firstSeen, &ob.FirstSeenAt},
		{lastSeen, &ob.LastSeenAt},
		{updated, &ob.UpdatedAt},
	} {
		if *field.target, err = parseTime(field.raw); err != nil {
			return nil, err
		}
	}
	if lastTrigger.Valid {
		if ob.LastTriggerAt, err = parseTime(lastTrigger.String); err != nil {
			return nil, err
		}
	}
	return &ob, nil
}

func writeRecord(ctx context.Context, tx *sql.Tx, rec Record) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO failure_cooldowns (fingerprint, cluster_id, subject_key, api_version, kind, namespace,
		                                name, uid, reason_class, scope, next_backoff_level, last_paged_at,
		                                cooldown_until, suppressed_count, first_seen_at, last_seen_at,
		                                last_resolved_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
		 ON CONFLICT (fingerprint) DO UPDATE SET
		     next_backoff_level = excluded.next_backoff_level,
		     last_paged_at = excluded.last_paged_at,
		     cooldown_until = excluded.cooldown_until,
		     suppressed_count = excluded.suppressed_count,
		     last_seen_at = excluded.last_seen_at,
		     last_resolved_at = NULL,
		     updated_at = excluded.updated_at`,
		rec.Fingerprint, rec.ClusterID, rec.SubjectKey, rec.APIVersion, rec.Kind, rec.Namespace,
		rec.Name, rec.UID, rec.ReasonClass, rec.Scope, rec.NextBackoffLevel, formatTime(rec.LastPagedAt),
		formatTime(rec.CooldownUntil), rec.SuppressedCount, formatTime(rec.FirstSeenAt),
		formatTime(rec.LastSeenAt), formatTime(rec.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("write the cooldown of fingerprint %s: %w", rec.Fingerprint, err)
	}
	return nil
}

// truncate bounds a note, which is evidence and never identity.
func truncate(note string) string {
	if len(note) <= maxNoteLength {
		return note
	}
	return note[:maxNoteLength]
}

// formatTime writes a time as UTC with nanosecond precision, matching the
// history package so the two share one column format.
func formatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// parseTime reads a time written by formatTime.
func parseTime(raw string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, raw)
}

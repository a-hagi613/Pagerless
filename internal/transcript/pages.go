package transcript

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Target is the optional Kubernetes resource hint of a page or a turn.
type Target struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Empty reports whether no target was supplied.
func (t Target) Empty() bool { return t.Kind == "" && t.Name == "" && t.APIVersion == "" }

// Page is one investigation conversation.
type Page struct {
	ID string
	// Title is the first message, shortened for a list.
	Title     string
	Status    string
	Namespace string
	Target    Target
	CreatedAt time.Time
	UpdatedAt time.Time
	// ActiveTurnID is empty when no turn is running.
	ActiveTurnID string
}

// Summary is the list projection of a page.
type Summary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ActiveTurnID *string   `json:"activeTurnId"`
}

// Summary renders the list projection.
func (p Page) Summary() Summary {
	summary := Summary{ID: p.ID, Title: p.Title, Status: p.Status, UpdatedAt: p.UpdatedAt}
	if p.ActiveTurnID != "" {
		active := p.ActiveTurnID
		summary.ActiveTurnID = &active
	}
	return summary
}

// keyStatus is the JSON field shared by several event payloads.
const keyStatus = "status"

// Turn is one request within a page. Namespace and Target are the resolved
// hints the turn ran with.
type Turn struct {
	ID             string
	PageID         string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	IdempotencyKey string
	Namespace      string
	Target         Target
	ErrorCode      string
	ErrorMessage   string
}

// Citation points at tool evidence a message relies on.
type Citation struct {
	CallID      string `json:"callId"`
	JSONPointer string `json:"jsonPointer,omitempty"`
}

// Message is one stored conversation entry.
type Message struct {
	ID        string
	PageID    string
	TurnID    string
	Role      string
	Content   string
	CreatedAt time.Time
	Citations []Citation
}

// Event is one entry of the append-only log.
type Event struct {
	Seq    int64           `json:"seq"`
	PageID string          `json:"pageId"`
	TurnID string          `json:"turnId"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Data   json.RawMessage `json:"data"`
}

// Evidence is one sanitized tool attempt.
type Evidence struct {
	PageID     string
	CallID     string
	Attempt    int
	TurnID     string
	Tool       string
	Arguments  json.RawMessage
	Result     json.RawMessage
	Status     string
	DurationMS int64
	Redacted   int
	Truncated  bool
	CreatedAt  time.Time
}

// FileDiff is one file of a proposal.
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// ProposedFile is the validated content of one proposed file.
type ProposedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Proposal is the stored outcome of the publish path for one turn.
type Proposal struct {
	PageID    string
	TurnID    string
	BaseSHA   string
	Branch    string
	PRURL     string
	Status    string
	Error     string
	Files     []ProposedFile
	Diff      []FileDiff
	CreatedAt time.Time
}

// Proposal statuses.
const (
	ProposalPublished = "published"
	ProposalRejected  = "rejected"
)

// NewID returns a fresh opaque identifier.
func NewID() string { return mustRandomID() }

func mustRandomID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic("transcript: the system random source failed: " + err.Error())
	}
	return hex.EncodeToString(buffer)
}

// CreatePage stores a new page, its first user message and its first turn in
// one transaction. It returns ErrConflict when the page already exists.
func (s *Store) CreatePage(ctx context.Context, page Page, message Message, turn Turn) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pages (id, title, status, namespace, target_kind, target_name,
			target_api_version, created_at, updated_at, active_turn_id, next_seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		page.ID, page.Title, page.Status, page.Namespace, page.Target.Kind, page.Target.Name,
		page.Target.APIVersion, formatTime(page.CreatedAt), formatTime(page.UpdatedAt),
		page.ActiveTurnID); err != nil {
		if isConstraint(err) {
			return fmt.Errorf("%w: the page %s exists", ErrConflict, page.ID)
		}
		return fmt.Errorf("insert the page: %w", err)
	}
	if err := insertTurn(ctx, tx, turn); err != nil {
		return err
	}
	if err := insertMessage(ctx, tx, message); err != nil {
		return err
	}
	event := Event{
		PageID: page.ID, TurnID: turn.ID, Type: EventTurnStarted, Time: turn.CreatedAt,
		Data: mustJSON(map[string]any{keyStatus: StatusRunning}),
	}
	if err := appendEvent(ctx, tx, &event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the page: %w", err)
	}
	s.notifyPage(page.ID)
	return nil
}

// AddTurn stores a follow-up user message and starts a turn. It refuses a page
// that already has an active turn with ErrTurnInProgress.
func (s *Store) AddTurn(
	ctx context.Context, pageID string, message Message, turn Turn, namespace string, target Target,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the turn: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		active   string
		existing sql.NullString
	)
	err = tx.QueryRowContext(ctx, `SELECT active_turn_id FROM pages WHERE id = ?`, pageID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the page %s", ErrNotFound, pageID)
	}
	if err != nil {
		return fmt.Errorf("read the page %s: %w", pageID, err)
	}
	active = existing.String
	if active != "" {
		return fmt.Errorf("%w: the page %s has the running turn %s", ErrTurnInProgress, pageID, active)
	}
	if err := insertTurn(ctx, tx, turn); err != nil {
		return err
	}
	if err := insertMessage(ctx, tx, message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pages SET status = ?, active_turn_id = ?, updated_at = ?, title = ?,
			namespace = ?, target_kind = ?, target_name = ?, target_api_version = ? WHERE id = ?`,
		StatusRunning, turn.ID, formatTime(turn.CreatedAt), titleOf(message.Content),
		namespace, target.Kind, target.Name, target.APIVersion, pageID); err != nil {
		return fmt.Errorf("start the turn of the page %s: %w", pageID, err)
	}
	event := Event{
		PageID: pageID, TurnID: turn.ID, Type: EventTurnStarted, Time: turn.CreatedAt,
		Data: mustJSON(map[string]any{keyStatus: StatusRunning}),
	}
	if err := appendEvent(ctx, tx, &event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the turn: %w", err)
	}
	s.notifyPage(pageID)
	return nil
}

func insertTurn(ctx context.Context, tx *sql.Tx, turn Turn) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO turns (id, page_id, status, created_at, updated_at, idempotency_key,
			namespace, target_kind, target_name, target_api_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turn.ID, turn.PageID, turn.Status, formatTime(turn.CreatedAt), formatTime(turn.UpdatedAt),
		turn.IdempotencyKey, turn.Namespace, turn.Target.Kind, turn.Target.Name, turn.Target.APIVersion); err != nil {
		if isConstraint(err) {
			return fmt.Errorf("%w: the turn %s exists", ErrConflict, turn.ID)
		}
		return fmt.Errorf("insert the turn: %w", err)
	}
	return nil
}

func insertMessage(ctx context.Context, tx *sql.Tx, message Message) error {
	citations, err := json.Marshal(message.Citations)
	if err != nil {
		return fmt.Errorf("encode the citations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, page_id, turn_id, role, content, citations, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.PageID, message.TurnID, message.Role, message.Content, string(citations),
		formatTime(message.CreatedAt)); err != nil {
		return fmt.Errorf("insert the message: %w", err)
	}
	return nil
}

// FinishTurn records the terminal status, the assistant message, and the
// turn.finished event in one transaction. The assistant message may be empty
// for a stopped or failed turn without content.
func (s *Store) FinishTurn(
	ctx context.Context, pageID, turnID, status string, message *Message, events []Event,
	errorCode, errorMessage string,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the turn outcome: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT status FROM turns WHERE id = ? AND page_id = ?`, turnID, pageID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return fmt.Errorf("read the turn %s: %w", turnID, err)
	}
	if current != StatusRunning {
		// A second completion, for example after a stop raced with the agent
		// result, is idempotent and keeps the first durable outcome.
		return nil
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE turns SET status = ?, updated_at = ?, error_code = ?, error_message = ? WHERE id = ?`,
		status, formatTime(now), errorCode, errorMessage, turnID); err != nil {
		return fmt.Errorf("finish the turn %s: %w", turnID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pages SET status = ?, active_turn_id = '', updated_at = ? WHERE id = ?`,
		status, formatTime(now), pageID); err != nil {
		return fmt.Errorf("finish the page %s: %w", pageID, err)
	}
	if message != nil {
		if err := insertMessage(ctx, tx, *message); err != nil {
			return err
		}
	}
	for index := range events {
		events[index].PageID = pageID
		events[index].TurnID = turnID
		if events[index].Time.IsZero() {
			events[index].Time = now
		}
		if err := appendEvent(ctx, tx, &events[index]); err != nil {
			return err
		}
	}
	finished := map[string]any{keyStatus: status}
	if errorCode != "" {
		finished["error"] = map[string]string{"code": errorCode, "message": errorMessage}
	}
	event := Event{
		PageID: pageID, TurnID: turnID, Type: EventTurnFinished, Time: now,
		Data: mustJSON(finished),
	}
	if err := appendEvent(ctx, tx, &event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the turn outcome: %w", err)
	}
	s.notifyPage(pageID)
	return nil
}

// AppendEvent appends one event to a page and returns its sequence number.
func (s *Store) AppendEvent(ctx context.Context, event Event) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin the event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := appendEvent(ctx, tx, &event); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit the event: %w", err)
	}
	s.notifyPage(event.PageID)
	return event.Seq, nil
}

// appendEvent assigns the next page sequence number inside tx.
func appendEvent(ctx context.Context, tx *sql.Tx, event *Event) error {
	var seq int64
	err := tx.QueryRowContext(ctx,
		`UPDATE pages SET next_seq = next_seq + 1, updated_at = ? WHERE id = ? RETURNING next_seq - 1`,
		formatTime(event.Time), event.PageID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: the page %s", ErrNotFound, event.PageID)
	}
	if err != nil {
		return fmt.Errorf("assign the event sequence of the page %s: %w", event.PageID, err)
	}
	event.Seq = seq
	if len(event.Data) == 0 {
		event.Data = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (page_id, seq, turn_id, type, time, data) VALUES (?, ?, ?, ?, ?, ?)`,
		event.PageID, event.Seq, event.TurnID, event.Type, formatTime(event.Time), string(event.Data)); err != nil {
		return fmt.Errorf("insert the event: %w", err)
	}
	return nil
}

// EventsAfter returns the page's events with a sequence number greater than
// after, in sequence order.
func (s *Store) EventsAfter(ctx context.Context, pageID string, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, page_id, turn_id, type, time, data FROM events
		 WHERE page_id = ? AND seq > ? ORDER BY seq`, pageID, after)
	if err != nil {
		return nil, fmt.Errorf("read the events of the page %s: %w", pageID, err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]Event, 0, 16)
	for rows.Next() {
		var (
			event Event
			at    string
			data  string
		)
		if err := rows.Scan(&event.Seq, &event.PageID, &event.TurnID, &event.Type, &at, &data); err != nil {
			return nil, fmt.Errorf("read an event of the page %s: %w", pageID, err)
		}
		if event.Time, err = parseTime(at); err != nil {
			return nil, err
		}
		event.Data = json.RawMessage(data)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the events of the page %s: %w", pageID, err)
	}
	return events, nil
}

// LastEventSeq returns the last durable sequence number of a page.
func (s *Store) LastEventSeq(ctx context.Context, pageID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT next_seq - 1 FROM pages WHERE id = ?`, pageID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: the page %s", ErrNotFound, pageID)
	}
	if err != nil {
		return 0, fmt.Errorf("read the last event of the page %s: %w", pageID, err)
	}
	return seq, nil
}

// Page returns one page.
func (s *Store) Page(ctx context.Context, pageID string) (Page, error) {
	var (
		page             Page
		target           Target
		created, updated string
		active           string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, status, namespace, target_kind, target_name, target_api_version,
			created_at, updated_at, active_turn_id FROM pages WHERE id = ?`, pageID,
	).Scan(&page.ID, &page.Title, &page.Status, &page.Namespace, &target.Kind, &target.Name,
		&target.APIVersion, &created, &updated, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Page{}, fmt.Errorf("%w: the page %s", ErrNotFound, pageID)
	}
	if err != nil {
		return Page{}, fmt.Errorf("read the page %s: %w", pageID, err)
	}
	if page.CreatedAt, err = parseTime(created); err != nil {
		return Page{}, err
	}
	if page.UpdatedAt, err = parseTime(updated); err != nil {
		return Page{}, err
	}
	page.Target = target
	page.ActiveTurnID = active
	return page, nil
}

// Pages returns page summaries, newest activity first. limit bounds the result.
func (s *Store) Pages(ctx context.Context, limit int) ([]Page, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, status, namespace, target_kind, target_name, target_api_version,
			created_at, updated_at, active_turn_id FROM pages
		 ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list the pages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pages := make([]Page, 0, limit)
	for rows.Next() {
		var (
			page             Page
			target           Target
			created, updated string
		)
		if err := rows.Scan(&page.ID, &page.Title, &page.Status, &page.Namespace, &target.Kind,
			&target.Name, &target.APIVersion, &created, &updated, &page.ActiveTurnID); err != nil {
			return nil, fmt.Errorf("read a page summary: %w", err)
		}
		if page.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if page.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		page.Target = target
		pages = append(pages, page)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the pages: %w", err)
	}
	return pages, nil
}

// Messages returns a page's messages in creation order.
func (s *Store) Messages(ctx context.Context, pageID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, page_id, turn_id, role, content, citations, created_at FROM messages
		 WHERE page_id = ? ORDER BY created_at, rowid`, pageID)
	if err != nil {
		return nil, fmt.Errorf("read the messages of the page %s: %w", pageID, err)
	}
	defer func() { _ = rows.Close() }()
	messages := make([]Message, 0, 8)
	for rows.Next() {
		var (
			message   Message
			citations string
			at        string
		)
		if err := rows.Scan(&message.ID, &message.PageID, &message.TurnID, &message.Role,
			&message.Content, &citations, &at); err != nil {
			return nil, fmt.Errorf("read a message of the page %s: %w", pageID, err)
		}
		if err := json.Unmarshal([]byte(citations), &message.Citations); err != nil {
			return nil, fmt.Errorf("decode the citations of message %s: %w", message.ID, err)
		}
		if message.CreatedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the messages of the page %s: %w", pageID, err)
	}
	return messages, nil
}

// Turn returns one turn.
func (s *Store) Turn(ctx context.Context, turnID string) (Turn, error) {
	var (
		turn             Turn
		created, updated string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, page_id, status, created_at, updated_at, idempotency_key, namespace,
			target_kind, target_name, target_api_version, error_code, error_message
		 FROM turns WHERE id = ?`, turnID,
	).Scan(&turn.ID, &turn.PageID, &turn.Status, &created, &updated, &turn.IdempotencyKey,
		&turn.Namespace, &turn.Target.Kind, &turn.Target.Name, &turn.Target.APIVersion,
		&turn.ErrorCode, &turn.ErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, fmt.Errorf("%w: the turn %s", ErrNotFound, turnID)
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read the turn %s: %w", turnID, err)
	}
	if turn.CreatedAt, err = parseTime(created); err != nil {
		return Turn{}, err
	}
	if turn.UpdatedAt, err = parseTime(updated); err != nil {
		return Turn{}, err
	}
	return turn, nil
}

// ActiveTurn returns the running turn of a page, if any.
func (s *Store) ActiveTurn(ctx context.Context, pageID string) (Turn, bool, error) {
	var turnID string
	err := s.db.QueryRowContext(ctx, `SELECT active_turn_id FROM pages WHERE id = ?`, pageID).Scan(&turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, false, fmt.Errorf("%w: the page %s", ErrNotFound, pageID)
	}
	if err != nil {
		return Turn{}, false, fmt.Errorf("read the active turn of the page %s: %w", pageID, err)
	}
	if turnID == "" {
		return Turn{}, false, nil
	}
	turn, err := s.Turn(ctx, turnID)
	if err != nil {
		return Turn{}, false, err
	}
	return turn, true, nil
}

// PutEvidence stores one sanitized tool attempt. A replay of the same call and
// attempt is idempotent, and a later attempt of the same call ID in another turn
// is stored beside the earlier one rather than replacing it.
func (s *Store) PutEvidence(ctx context.Context, evidence Evidence) (Evidence, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Evidence{}, fmt.Errorf("begin the evidence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing Evidence
	if found, err := evidenceByAttempt(ctx, tx, evidence.PageID, evidence.CallID, evidence.Attempt); err != nil {
		return Evidence{}, err
	} else if found {
		return existing, nil
	}
	if evidence.Attempt <= 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(attempt), 0) + 1 FROM evidence WHERE page_id = ? AND call_id = ?`,
			evidence.PageID, evidence.CallID).Scan(&evidence.Attempt); err != nil {
			return Evidence{}, fmt.Errorf("assign the evidence attempt: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO evidence (page_id, call_id, attempt, turn_id, tool, arguments, result, status,
			duration_ms, redacted, truncated, bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evidence.PageID, evidence.CallID, evidence.Attempt, evidence.TurnID, evidence.Tool,
		string(evidence.Arguments), string(evidence.Result), evidence.Status, evidence.DurationMS,
		evidence.Redacted, boolInt(evidence.Truncated), len(evidence.Result),
		formatTime(evidence.CreatedAt)); err != nil {
		return Evidence{}, fmt.Errorf("insert the evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Evidence{}, fmt.Errorf("commit the evidence: %w", err)
	}
	return evidence, nil
}

// Evidence returns the newest attempt of one call on a page.
func (s *Store) Evidence(ctx context.Context, pageID, callID string) (Evidence, error) {
	var (
		evidence  Evidence
		at        string
		arguments string
		result    string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT page_id, call_id, attempt, turn_id, tool, arguments, result, status, duration_ms,
			redacted, truncated, created_at FROM evidence
		 WHERE page_id = ? AND call_id = ? ORDER BY attempt DESC LIMIT 1`, pageID, callID,
	).Scan(&evidence.PageID, &evidence.CallID, &evidence.Attempt, &evidence.TurnID, &evidence.Tool,
		&arguments, &result, &evidence.Status, &evidence.DurationMS,
		&evidence.Redacted, &evidence.Truncated, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Evidence{}, fmt.Errorf("%w: the evidence %s", ErrNotFound, callID)
	}
	if err != nil {
		return Evidence{}, fmt.Errorf("read the evidence %s: %w", callID, err)
	}
	if evidence.CreatedAt, err = parseTime(at); err != nil {
		return Evidence{}, err
	}
	evidence.Arguments = json.RawMessage(arguments)
	evidence.Result = json.RawMessage(result)
	return evidence, nil
}

// AttemptOf returns the attempt number of a call in a turn, and whether one
// exists. The callback path uses it to make a retried callback idempotent.
func (s *Store) AttemptOf(ctx context.Context, pageID, turnID, callID string) (int, bool, error) {
	var attempt int
	err := s.db.QueryRowContext(ctx,
		`SELECT attempt FROM evidence WHERE page_id = ? AND turn_id = ? AND call_id = ?
		 ORDER BY attempt DESC LIMIT 1`, pageID, turnID, callID).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the evidence attempt: %w", err)
	}
	return attempt, true, nil
}

// EvidenceInTurn returns the newest attempt of a call in one turn.
func (s *Store) EvidenceInTurn(ctx context.Context, pageID, turnID, callID string) (Evidence, bool, error) {
	var (
		evidence  Evidence
		at        string
		arguments string
		result    string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT page_id, call_id, attempt, turn_id, tool, arguments, result, status, duration_ms,
			redacted, truncated, created_at FROM evidence
		 WHERE page_id = ? AND turn_id = ? AND call_id = ? ORDER BY attempt DESC LIMIT 1`,
		pageID, turnID, callID,
	).Scan(&evidence.PageID, &evidence.CallID, &evidence.Attempt, &evidence.TurnID, &evidence.Tool,
		&arguments, &result, &evidence.Status, &evidence.DurationMS,
		&evidence.Redacted, &evidence.Truncated, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Evidence{}, false, nil
	}
	if err != nil {
		return Evidence{}, false, fmt.Errorf("read the evidence %s: %w", callID, err)
	}
	if evidence.CreatedAt, err = parseTime(at); err != nil {
		return Evidence{}, false, err
	}
	evidence.Arguments = json.RawMessage(arguments)
	evidence.Result = json.RawMessage(result)
	return evidence, true, nil
}

// EvidenceForTurn returns every attempt a turn made, in creation order.
func (s *Store) EvidenceForTurn(ctx context.Context, pageID, turnID string) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT page_id, call_id, attempt, turn_id, tool, arguments, result, status, duration_ms,
			redacted, truncated FROM evidence WHERE page_id = ? AND turn_id = ?
		 ORDER BY created_at, attempt`, pageID, turnID)
	if err != nil {
		return nil, fmt.Errorf("read the evidence of the turn %s: %w", turnID, err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]Evidence, 0, 8)
	for rows.Next() {
		var (
			evidence  Evidence
			arguments string
			result    string
		)
		if err := rows.Scan(&evidence.PageID, &evidence.CallID, &evidence.Attempt, &evidence.TurnID,
			&evidence.Tool, &arguments, &result, &evidence.Status,
			&evidence.DurationMS, &evidence.Redacted, &evidence.Truncated); err != nil {
			return nil, fmt.Errorf("read an evidence of the turn %s: %w", turnID, err)
		}
		evidence.Arguments = json.RawMessage(arguments)
		evidence.Result = json.RawMessage(result)
		items = append(items, evidence)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the evidence of the turn %s: %w", turnID, err)
	}
	return items, nil
}

func evidenceByAttempt(ctx context.Context, tx *sql.Tx, pageID, callID string, attempt int) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM evidence WHERE page_id = ? AND call_id = ? AND attempt = ?`,
		pageID, callID, attempt).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the evidence attempt: %w", err)
	}
	return true, nil
}

// SaveProposal stores the outcome of the publish path for one turn.
func (s *Store) SaveProposal(ctx context.Context, proposal Proposal) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	files, err := json.Marshal(proposal.Files)
	if err != nil {
		return fmt.Errorf("encode the proposal files: %w", err)
	}
	diff, err := json.Marshal(proposal.Diff)
	if err != nil {
		return fmt.Errorf("encode the proposal diff: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO proposals (page_id, turn_id, base_sha, branch, pr_url, status, error, files, diff, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(page_id, turn_id) DO UPDATE SET
		   base_sha = excluded.base_sha, branch = excluded.branch, pr_url = excluded.pr_url,
		   status = excluded.status, error = excluded.error, files = excluded.files,
		   diff = excluded.diff, created_at = excluded.created_at`,
		proposal.PageID, proposal.TurnID, proposal.BaseSHA, proposal.Branch, proposal.PRURL,
		proposal.Status, proposal.Error, string(files), string(diff),
		formatTime(proposal.CreatedAt)); err != nil {
		return fmt.Errorf("store the proposal of the turn %s: %w", proposal.TurnID, err)
	}
	return nil
}

// Proposal returns the newest proposal of a page.
func (s *Store) Proposal(ctx context.Context, pageID string) (Proposal, bool, error) {
	var (
		proposal Proposal
		files    string
		diff     string
		at       string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT page_id, turn_id, base_sha, branch, pr_url, status, error, files, diff, created_at
		 FROM proposals WHERE page_id = ? ORDER BY created_at DESC LIMIT 1`, pageID,
	).Scan(&proposal.PageID, &proposal.TurnID, &proposal.BaseSHA, &proposal.Branch, &proposal.PRURL,
		&proposal.Status, &proposal.Error, &files, &diff, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, false, nil
	}
	if err != nil {
		return Proposal{}, false, fmt.Errorf("read the proposal of the page %s: %w", pageID, err)
	}
	if err := json.Unmarshal([]byte(files), &proposal.Files); err != nil {
		return Proposal{}, false, fmt.Errorf("decode the proposal files: %w", err)
	}
	if err := json.Unmarshal([]byte(diff), &proposal.Diff); err != nil {
		return Proposal{}, false, fmt.Errorf("decode the proposal diff: %w", err)
	}
	if proposal.CreatedAt, err = parseTime(at); err != nil {
		return Proposal{}, false, err
	}
	return proposal, true, nil
}

// Idempotent returns the stored response and request hash for a key, operation
// and page.
func (s *Store) Idempotent(ctx context.Context, key, operation, pageID string) ([]byte, string, bool, error) {
	var response, hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT response, request_hash FROM idempotency WHERE key = ? AND operation = ? AND page_id = ?`,
		key, operation, pageID).Scan(&response, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("read the idempotency record: %w", err)
	}
	return []byte(response), hash, true, nil
}

// SaveIdempotent stores the response for a key. A concurrent duplicate loses
// the insert race and the caller reads the winner instead.
func (s *Store) SaveIdempotent(
	ctx context.Context, key, operation, pageID, turnID, requestHash string, response []byte,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO idempotency (key, operation, page_id, turn_id, request_hash, response, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`,
		key, operation, pageID, turnID, requestHash, string(response), formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("store the idempotency record: %w", err)
	}
	return nil
}

// InterruptRunningTurns marks every running turn as interrupted after a
// restart. A turn cannot survive the process that owns its goroutine, so the
// durable record must say so rather than leaving a spinner forever.
func (s *Store) InterruptRunningTurns(ctx context.Context) ([]Turn, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin the interruption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, page_id, status, created_at, updated_at, idempotency_key, namespace,
			target_kind, target_name, target_api_version FROM turns WHERE status = ?`,
		StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("read the running turns: %w", err)
	}
	turns := make([]Turn, 0, 4)
	for rows.Next() {
		var (
			turn             Turn
			created, updated string
		)
		if err := rows.Scan(&turn.ID, &turn.PageID, &turn.Status, &created, &updated,
			&turn.IdempotencyKey, &turn.Namespace, &turn.Target.Kind, &turn.Target.Name,
			&turn.Target.APIVersion); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read a running turn: %w", err)
		}
		if turn.CreatedAt, err = parseTime(created); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if turn.UpdatedAt, err = parseTime(updated); err != nil {
			_ = rows.Close()
			return nil, err
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read the running turns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read the running turns: %w", err)
	}
	now := time.Now().UTC()
	for _, turn := range turns {
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET status = ?, updated_at = ?, error_code = ?, error_message = ? WHERE id = ?`,
			StatusInterrupted, formatTime(now), "interrupted",
			"The server restarted before the turn finished.", turn.ID); err != nil {
			return nil, fmt.Errorf("interrupt the turn %s: %w", turn.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE pages SET status = ?, active_turn_id = '', updated_at = ? WHERE id = ?`,
			StatusInterrupted, formatTime(now), turn.PageID); err != nil {
			return nil, fmt.Errorf("interrupt the page %s: %w", turn.PageID, err)
		}
		event := Event{
			PageID: turn.PageID, TurnID: turn.ID, Type: EventTurnFinished, Time: now,
			Data: mustJSON(map[string]any{
				"status": StatusInterrupted,
				"reason": "the server restarted before the turn finished",
			}),
		}
		if err := appendEvent(ctx, tx, &event); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit the interruption: %w", err)
	}
	for _, turn := range turns {
		s.notifyPage(turn.PageID)
	}
	return turns, nil
}

// DeleteExpired removes pages whose last activity is older than cutoff. A page
// with a running turn is never deleted, so retention cannot cut a live answer.
func (s *Store) DeleteExpired(ctx context.Context, cutoff time.Time) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM pages WHERE updated_at < ? AND active_turn_id = ''`, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("delete the expired pages: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the deleted pages: %w", err)
	}
	return int(removed), nil
}

// Ping verifies the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// titleOf shortens a first message into a page title.
func titleOf(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	runes := []rune(line)
	if len(runes) > 120 {
		return string(runes[:117]) + "..."
	}
	if len(runes) == 0 {
		return "Untitled investigation"
	}
	return string(runes)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("transcript: encode a fixed shape: " + err.Error())
	}
	return encoded
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func isConstraint(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "constraint") || strings.Contains(text, "unique")
}

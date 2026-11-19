package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ToolRecord is one tool call the broker served, and the redacted result the
// model saw. It is what a Finding's citation is verified against.
type ToolRecord struct {
	// PageUID is the UID of the Page the call belongs to.
	PageUID string
	// CallID is the identifier a Finding cites.
	CallID string
	// Tool is the catalogue name that ran.
	Tool string
	// Arguments is the bounded rendering of the model's arguments.
	Arguments string
	// Data is the redacted JSON result.
	Data []byte
	// Outcome is ok, out_of_scope, unknown_tool, budget_exhausted, or tool_error.
	Outcome string
	// Bytes is the size of Data.
	Bytes int64
	// Redactions is the number of matches removed from Data.
	Redactions int
	// RecordedAt is when the broker served the call.
	RecordedAt time.Time
}

// PutToolRecord stores one call. A retry with the same Page and call identity
// replaces the earlier row rather than counting a second read.
func (s *Store) PutToolRecord(ctx context.Context, record ToolRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO page_evidence
		 (page_uid, call_id, tool, arguments, data, outcome, bytes, redactions, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(page_uid, call_id) DO UPDATE SET
		   tool = excluded.tool, arguments = excluded.arguments, data = excluded.data,
		   outcome = excluded.outcome, bytes = excluded.bytes, redactions = excluded.redactions,
		   recorded_at = excluded.recorded_at`,
		record.PageUID, record.CallID, record.Tool, record.Arguments, record.Data, record.Outcome,
		record.Bytes, record.Redactions, formatTime(record.RecordedAt),
	)
	if err != nil {
		return fmt.Errorf("store the tool call %s of Page %s: %w", record.CallID, record.PageUID, err)
	}
	return nil
}

// ToolRecord returns one stored call. found is false when the Page never made it.
func (s *Store) ToolRecord(ctx context.Context, pageUID, callID string) (ToolRecord, bool, error) {
	var (
		record   ToolRecord
		recorded string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT page_uid, call_id, tool, arguments, data, outcome, bytes, redactions, recorded_at
		 FROM page_evidence WHERE page_uid = ? AND call_id = ?`, pageUID, callID,
	).Scan(&record.PageUID, &record.CallID, &record.Tool, &record.Arguments, &record.Data,
		&record.Outcome, &record.Bytes, &record.Redactions, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolRecord{}, false, nil
	}
	if err != nil {
		return ToolRecord{}, false, fmt.Errorf("read tool call %s of Page %s: %w", callID, pageUID, err)
	}
	if record.RecordedAt, err = parseTime(recorded); err != nil {
		return ToolRecord{}, false, fmt.Errorf("read the time of tool call %s: %w", callID, err)
	}
	return record, true, nil
}

// ToolCallStats returns the number of calls and the bytes of results the Page
// has been served.
func (s *Store) ToolCallStats(ctx context.Context, pageUID string) (int, int64, error) {
	var (
		count int
		bytes int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM page_evidence WHERE page_uid = ? AND outcome = 'ok'`,
		pageUID).Scan(&count, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("read the tool-call usage of Page %s: %w", pageUID, err)
	}
	return count, bytes, nil
}

// ToolRefusals returns the number of times a Page asked for one refused name.
func (s *Store) ToolRefusals(ctx context.Context, pageUID, tool string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM page_evidence WHERE page_uid = ? AND tool = ? AND outcome <> 'ok'`,
		pageUID, tool).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("read the refused tool calls of Page %s: %w", pageUID, err)
	}
	return count, nil
}

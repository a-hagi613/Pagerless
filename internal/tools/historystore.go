package tools

import (
	"context"
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/a-hagi613/pagerless/internal/history"
)

// HistoryStore is the SQLite-backed evidence store. It keeps a tool result
// across a server restart, so a Finding's citation outlives the process that
// served it.
type HistoryStore struct {
	// Store is the history database the server already writes.
	Store *history.Store
	// Clock stamps a record that was stored without a time. It defaults to the real clock.
	Clock clock.PassiveClock
	// refusals counts refused names in process. It is deliberately not durable:
	// it exists so one run's repeated attempts raise one SuspiciousOutput, and a
	// restarted server simply starts the count again.
	mu       sync.Mutex
	refusals map[string]int
}

// now returns the store's clock reading.
func (h *HistoryStore) now() time.Time {
	if h.Clock == nil {
		return time.Now()
	}
	return h.Clock.Now()
}

// Record stores one call.
func (h *HistoryStore) Record(ctx context.Context, record Record) error {
	return h.Store.PutToolRecord(ctx, history.ToolRecord{
		PageUID: record.PageUID, CallID: record.CallID, Tool: record.Tool, Arguments: record.Arguments,
		Data: record.Data, Outcome: record.Outcome, Bytes: record.Bytes, Redactions: record.Redactions,
		RecordedAt: record.At,
	})
}

// Get returns one stored call.
func (h *HistoryStore) Get(ctx context.Context, pageUID, callID string) (Record, bool, error) {
	stored, found, err := h.Store.ToolRecord(ctx, pageUID, callID)
	if err != nil || !found {
		return Record{}, found, err
	}
	record := Record{
		PageUID: stored.PageUID, CallID: stored.CallID, Tool: stored.Tool, Arguments: stored.Arguments,
		Data: stored.Data, Outcome: stored.Outcome, Bytes: stored.Bytes, Redactions: stored.Redactions,
		At: stored.RecordedAt,
	}
	if record.At.IsZero() {
		record.At = h.now()
	}
	return record, true, nil
}

// Served returns the number of successful calls the Page has made.
func (h *HistoryStore) Served(ctx context.Context, pageUID string) (int, error) {
	count, _, err := h.Store.ToolCallStats(ctx, pageUID)
	return count, err
}

// Bytes returns the bytes of results the Page has been served.
func (h *HistoryStore) Bytes(ctx context.Context, pageUID string) (int64, error) {
	_, bytes, err := h.Store.ToolCallStats(ctx, pageUID)
	return bytes, err
}

// Refusals increments and returns how often the Page asked for one refused name.
func (h *HistoryStore) Refusals(_ context.Context, pageUID, tool string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refusals == nil {
		h.refusals = map[string]int{}
	}
	key := pageUID + "\x00" + tool
	h.refusals[key]++
	return h.refusals[key], nil
}

package webhooks

import (
	"context"
	"time"

	"github.com/a-hagi613/pagerless/internal/history"
)

// HistoryDeliveries persists delivery keys in the server's SQLite database.
type HistoryDeliveries struct {
	// Store is the history database.
	Store *history.Store
}

// HasDelivery reports whether the key is already recorded.
func (h *HistoryDeliveries) HasDelivery(ctx context.Context, key string) (bool, error) {
	return h.Store.HasDelivery(ctx, key)
}

// RecordDelivery records the key once.
func (h *HistoryDeliveries) RecordDelivery(ctx context.Context, delivery Delivery) (bool, error) {
	return h.Store.RecordDelivery(ctx, history.Delivery{
		Key: delivery.Key, Source: delivery.Source, Sender: delivery.Sender,
		Fingerprint: delivery.Fingerprint, Status: delivery.Status, Subject: delivery.Subject,
		Page: delivery.Page, ReceivedAt: delivery.ReceivedAt, Outcome: delivery.Outcome, Reason: delivery.Reason,
	})
}

// PruneDeliveries deletes rows received before the given time.
func (h *HistoryDeliveries) PruneDeliveries(ctx context.Context, before time.Time) (int64, error) {
	return h.Store.PruneDeliveries(ctx, before)
}

package contextbundle

import (
	"context"
	"errors"

	"github.com/a-hagi613/pagerless/internal/history"
)

// HistoryStore persists bundles in the server's SQLite database. The database
// keeps the first bundle of a Page, so a repeated assembly never replaces the
// evidence an investigation started from.
type HistoryStore struct {
	// Store is the history database.
	Store *history.Store
}

// Put stores the bundle once per Page.
func (h *HistoryStore) Put(ctx context.Context, bundle BundleRecord) error {
	err := h.Store.PutBundle(ctx, history.Bundle{
		PageUID: bundle.PageUID, Namespace: bundle.Namespace, Page: bundle.Page, Hash: bundle.Hash,
		Bytes: bundle.Bytes, Truncated: bundle.Truncated, CollectedAt: bundle.CollectedAt, Body: bundle.Body,
	})
	if errors.Is(err, history.ErrBundleExists) {
		return nil
	}
	return err
}

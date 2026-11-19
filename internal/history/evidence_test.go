package history

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Fixtures shared by the history tests.
const (
	testPage      = "checkout-abc"
	testNamespace = "pagerless"
)

// openTestStore opens a warehouse in a temporary directory.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close history: %v", err)
		}
	})
	return store
}

func TestABundleIsStoredOncePerPage(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	first := Bundle{
		PageUID: "uid-1", Namespace: testNamespace, Page: testPage, Hash: "aa",
		Bytes: 42, Truncated: true, CollectedAt: at, Body: `{"version":1}`,
	}
	if err := store.PutBundle(ctx, first); err != nil {
		t.Fatalf("put the first bundle: %v", err)
	}
	if err := store.PutBundle(ctx, Bundle{
		PageUID: "uid-1", Namespace: testNamespace, Page: testPage, Hash: "bb", Body: `{"version":2}`,
	}); !errors.Is(err, ErrBundleExists) {
		t.Fatalf("second put error = %v, want ErrBundleExists", err)
	}
	stored, found, err := store.Bundle(ctx, "uid-1")
	if err != nil || !found {
		t.Fatalf("read the bundle: found=%v err=%v", found, err)
	}
	if stored.Hash != "aa" || stored.Body != `{"version":1}` || !stored.Truncated || stored.Bytes != 42 {
		t.Errorf("stored bundle = %+v, want the first bundle unchanged", stored)
	}
	if !stored.CollectedAt.Equal(at) {
		t.Errorf("collected at = %s, want %s", stored.CollectedAt, at)
	}
	if _, found, err := store.Bundle(ctx, "missing"); err != nil || found {
		t.Errorf("missing bundle: found=%v err=%v, want false and nil", found, err)
	}
}

func TestADeliveryKeyIsIdempotentAndPrunable(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	delivery := Delivery{
		Key: "delivery-1", Source: "alertmanager", Sender: "alertmanager", Fingerprint: "fp",
		Status: "firing", Subject: "subject", Page: testPage, ReceivedAt: now, Outcome: "applied",
	}
	inserted, err := store.RecordDelivery(ctx, delivery)
	if err != nil || !inserted {
		t.Fatalf("record the first delivery: inserted=%v err=%v", inserted, err)
	}
	inserted, err = store.RecordDelivery(ctx, delivery)
	if err != nil || inserted {
		t.Fatalf("record the duplicate: inserted=%v err=%v, want false", inserted, err)
	}
	if found, err := store.HasDelivery(ctx, "delivery-1"); err != nil || !found {
		t.Errorf("HasDelivery = %v, %v, want true", found, err)
	}
	if found, err := store.HasDelivery(ctx, "missing"); err != nil || found {
		t.Errorf("HasDelivery of a missing key = %v, %v, want false", found, err)
	}
	removed, err := store.PruneDeliveries(ctx, now.Add(time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("prune = %d, %v, want 1 row", removed, err)
	}
	if found, err := store.HasDelivery(ctx, "delivery-1"); err != nil || found {
		t.Errorf("the pruned key is still present: %v, %v", found, err)
	}
	removed, err = store.PruneDeliveries(ctx, now.Add(2*time.Hour))
	if err != nil || removed != 0 {
		t.Errorf("second prune = %d, %v, want no rows", removed, err)
	}
}

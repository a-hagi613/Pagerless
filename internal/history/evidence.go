package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrBundleExists means the Page already has a stored Context Bundle. A bundle
// is stored once per Page, so a second write is not an error condition for the
// caller.
var ErrBundleExists = errors.New("the Page already has a Context Bundle")

// Bundle is one stored Context Bundle. The body is the sanitized JSON that
// contextbundle.Encode produced; it never enters the Page object.
type Bundle struct {
	// PageUID is the UID of the Page the bundle belongs to.
	PageUID string
	// Namespace is the Page namespace.
	Namespace string
	// Page is the Page name.
	Page string
	// Hash is the SHA-256 of the body.
	Hash string
	// Bytes is the body size.
	Bytes int64
	// Truncated reports whether a cap dropped evidence.
	Truncated bool
	// CollectedAt is when collection started.
	CollectedAt time.Time
	// Body is the encoded, sanitized bundle.
	Body string
}

// PutBundle stores a Context Bundle once per Page. It returns ErrBundleExists
// when the Page already has one, so the first deterministic assembly wins.
func (s *Store) PutBundle(ctx context.Context, bundle Bundle) error {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO `+bundlesTable+`
		 (page_uid, namespace, name, hash, bytes, truncated, collected_at, body)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(page_uid) DO NOTHING`,
		bundle.PageUID, bundle.Namespace, bundle.Page, bundle.Hash, bundle.Bytes,
		boolToInt(bundle.Truncated), formatTime(bundle.CollectedAt), bundle.Body,
	)
	if err != nil {
		return fmt.Errorf("store the Context Bundle of Page %s: %w", bundle.Page, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store the Context Bundle of Page %s: %w", bundle.Page, err)
	}
	if inserted == 0 {
		return ErrBundleExists
	}
	return nil
}

// Bundle returns the stored Context Bundle of a Page UID. found is false when
// the Page has none.
func (s *Store) Bundle(ctx context.Context, pageUID string) (Bundle, bool, error) {
	var (
		bundle    Bundle
		truncated int
		collected string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT page_uid, namespace, name, hash, bytes, truncated, collected_at, body
		 FROM `+bundlesTable+` WHERE page_uid = ?`, pageUID,
	).Scan(&bundle.PageUID, &bundle.Namespace, &bundle.Page, &bundle.Hash, &bundle.Bytes,
		&truncated, &collected, &bundle.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return Bundle{}, false, nil
	}
	if err != nil {
		return Bundle{}, false, fmt.Errorf("read the Context Bundle of Page %s: %w", pageUID, err)
	}
	bundle.Truncated = truncated != 0
	if bundle.CollectedAt, err = parseTime(collected); err != nil {
		return Bundle{}, false, fmt.Errorf("read the collection time of Page %s: %w", pageUID, err)
	}
	return bundle, true, nil
}

// Delivery is the durable idempotency record of one inbound alert.
type Delivery struct {
	// Key is the delivery key over source, sender fingerprint, status, start
	// time, and subject.
	Key string
	// Source is the sender dialect.
	Source string
	// Sender names the configured sender.
	Sender string
	// Fingerprint is the sender's alert fingerprint.
	Fingerprint string
	// Status is the alert status, firing or resolved.
	Status string
	// Subject is the resolved subject key, or the ignore reason when the alert
	// named no subject.
	Subject string
	// Page is the Page the alert opened or joined. It is empty when the alert
	// was ignored.
	Page string
	// ReceivedAt is when the server accepted the delivery.
	ReceivedAt time.Time
	// Outcome is applied or ignored.
	Outcome string
	// Reason explains an ignored delivery.
	Reason string
}

// HasDelivery reports whether a delivery key is already recorded.
func (s *Store) HasDelivery(ctx context.Context, key string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM `+deliveriesTable+` WHERE delivery_key = ?`, key).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read a webhook delivery key: %w", err)
	}
	return true, nil
}

// RecordDelivery stores one delivery and reports whether it is new. A repeated
// delivery key returns false without writing a second row, which is how a
// sender's retry returns success without a second Trigger.
func (s *Store) RecordDelivery(ctx context.Context, delivery Delivery) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO `+deliveriesTable+`
		 (delivery_key, source, sender, fingerprint, status, subject, page, received_at, outcome, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(delivery_key) DO NOTHING`,
		delivery.Key, delivery.Source, delivery.Sender, delivery.Fingerprint, delivery.Status,
		delivery.Subject, delivery.Page, formatTime(delivery.ReceivedAt), delivery.Outcome,
		nullIfEmpty(delivery.Reason),
	)
	if err != nil {
		return false, fmt.Errorf("record a webhook delivery: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record a webhook delivery: %w", err)
	}
	return inserted == 1, nil
}

// PruneDeliveries deletes deliveries received before the given time and returns
// how many rows it removed. The caller supplies the one-hour window.
func (s *Store) PruneDeliveries(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM `+deliveriesTable+` WHERE received_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune webhook deliveries: %w", err)
	}
	return removed, nil
}

// boolToInt stores a boolean as SQLite's integer.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// nullIfEmpty stores an empty string as NULL.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

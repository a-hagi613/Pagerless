package history

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// proposalHash matches a lowercase hexadecimal SHA-256 digest.
var proposalHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SaveProposalContents records the complete content of a Proposal's files under
// the Proposal hash. The table is append-only and the key is (hash, path), so
// saving the same Proposal again is a no-op rather than a rewrite.
func (s *Store) SaveProposalContents(
	ctx context.Context, hash string, files map[string][]byte, at time.Time,
) error {
	if !proposalHash.MatchString(hash) {
		return errors.New("the Proposal hash is not a lowercase hexadecimal SHA-256 digest")
	}
	if len(files) == 0 {
		return errors.New("the Proposal has no file content")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin saving the Proposal content: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for path, content := range files {
		if path == "" {
			return errors.New("a Proposal file has no path")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO `+contentsTable+` (proposal_hash, path, content, recorded_at)
			 VALUES (?, ?, ?, ?)`,
			hash, path, content, formatTime(at),
		); err != nil {
			return fmt.Errorf("save the content of %s: %w", path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the Proposal content: %w", err)
	}
	return nil
}

// ProposalContents returns the complete content of a Proposal's files. A hash
// that was never saved returns an empty map, not an error, so a caller can tell
// "not stored" from "unreadable".
func (s *Store) ProposalContents(ctx context.Context, hash string) (map[string][]byte, error) {
	if !proposalHash.MatchString(hash) {
		return nil, errors.New("the Proposal hash is not a lowercase hexadecimal SHA-256 digest")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, content FROM `+contentsTable+` WHERE proposal_hash = ?`, hash)
	if err != nil {
		return nil, fmt.Errorf("read the Proposal content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	files := map[string][]byte{}
	for rows.Next() {
		var (
			path    string
			content []byte
		)
		if err := rows.Scan(&path, &content); err != nil {
			return nil, fmt.Errorf("read a Proposal file: %w", err)
		}
		files[path] = content
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Proposal content: %w", err)
	}
	return files, nil
}

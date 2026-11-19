package history

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contentHash is a deterministic Proposal hash.
const contentHash = "abababababababababababababababababababababababababababababababab"

func TestProposalContentsAreSavedAndRead(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	files := map[string][]byte{
		"apps/checkout/deployment.yaml": []byte("kind: Deployment\n"),
		"apps/checkout/values.yaml":     []byte("replicas: 3\n"),
	}
	if err := store.SaveProposalContents(ctx, contentHash, files, startTime); err != nil {
		t.Fatalf("SaveProposalContents: %v", err)
	}
	// Saving the same Proposal again is a no-op, so a retry after a crash is safe.
	if err := store.SaveProposalContents(ctx, contentHash, files, startTime.Add(time.Minute)); err != nil {
		t.Fatalf("SaveProposalContents again: %v", err)
	}
	got, err := store.ProposalContents(ctx, contentHash)
	if err != nil {
		t.Fatalf("ProposalContents: %v", err)
	}
	if len(got) != len(files) {
		t.Fatalf("files = %d, want %d", len(got), len(files))
	}
	for path, content := range files {
		if string(got[path]) != string(content) {
			t.Errorf("%s = %q, want %q", path, got[path], content)
		}
	}
	unknown, err := store.ProposalContents(ctx, strings.Repeat("0", 64))
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown hash = %v, %v, want an empty map", unknown, err)
	}
}

func TestProposalContentsAreAppendOnly(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.SaveProposalContents(ctx, contentHash, map[string][]byte{"a.yaml": []byte("a")}, startTime); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE `+contentsTable+` SET content = 'b'`); err == nil {
		t.Error("UPDATE changed the Proposal content")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+contentsTable); err == nil {
		t.Error("DELETE removed the Proposal content")
	}
}

func TestSaveProposalContentsValidatesItsInput(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.SaveProposalContents(ctx, "not-a-hash", map[string][]byte{"a": []byte("a")}, startTime); err == nil {
		t.Error("an invalid hash was saved")
	}
	if err := store.SaveProposalContents(ctx, contentHash, nil, startTime); err == nil {
		t.Error("an empty Proposal was saved")
	}
	if _, err := store.ProposalContents(ctx, "not-a-hash"); err == nil {
		t.Error("an invalid hash was read")
	}
}

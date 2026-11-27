package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cursorStateDir returns the per-user state directory that remembers how far
// each page was read, so a disconnect can resume without replaying everything.
func cursorStateDir() string {
	if configured := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); configured != "" {
		return filepath.Join(configured, "pagerless")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "pagerless")
}

// cursorFile maps an opaque page ID to a fixed-length file name, so an ID that
// contains a separator or a traversal cannot pick the path.
func cursorFile(pageID string) (string, bool) {
	dir := cursorStateDir()
	if dir == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(pageID))
	name := hex.EncodeToString(sum[:]) + ".seq"
	return filepath.Join(dir, "cursors", name), true
}

// loadCursor returns the last fully read sequence for a page.
func loadCursor(pageID string) (int64, bool) {
	path, ok := cursorFile(pageID)
	if !ok {
		return 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || seq < 1 {
		return 0, false
	}
	return seq, true
}

// saveCursor records the last fully read sequence. A failure only costs a
// replay, so callers may ignore it.
func saveCursor(pageID string, seq int64) error {
	if seq < 1 {
		return nil
	}
	path, ok := cursorFile(pageID)
	if !ok {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return err
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return err
	}
	if _, err := file.WriteString(strconv.FormatInt(seq, 10) + "\n"); err != nil {
		_ = file.Close()
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// startingSequence chooses where a follow begins: an explicit --after, the
// beginning, or the stored resume point.
func startingSequence(pageID string, after int64, fromStart bool) int64 {
	if after >= 0 {
		return after
	}
	if fromStart {
		return 0
	}
	if seq, ok := loadCursor(pageID); ok {
		return seq
	}
	return 0
}

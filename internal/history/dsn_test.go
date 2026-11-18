package history

import (
	"strings"
	"testing"
)

// A Windows drive path is not a URI path. Without the leading slash the drive
// letter parses as a URI authority, and the SQLite driver rejects the DSN with
// "invalid uri authority".
func TestDataSourceNameAcceptsAWindowsPath(t *testing.T) {
	got := dataSourceName(`C:\Users\a b\history.db`, true)
	want := "file:///C:/Users/a%20b/history.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	if got != want {
		t.Fatalf("dataSourceName = %q, want %q", got, want)
	}
}

func TestDataSourceNameAcceptsAPOSIXPath(t *testing.T) {
	got := dataSourceName("/var/lib/pagerless/history.db", false)
	want := "file:///var/lib/pagerless/history.db?_pragma=busy_timeout(5000)"
	if got != want {
		t.Fatalf("dataSourceName = %q, want %q", got, want)
	}
}

// A reader must not enable the journal-mode pragma, which writes to the file.
func TestDataSourceNameOnlySetsTheJournalModeForTheWriter(t *testing.T) {
	if got := dataSourceName("/tmp/history.db", false); strings.Contains(got, "journal_mode") {
		t.Fatalf("reader DSN carries journal_mode: %q", got)
	}
}

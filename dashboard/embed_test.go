package dashboard

import (
	"io/fs"
	"strings"
	"testing"
)

func TestTheBuildDirectoryIsEmbedded(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(Assets(), "dist")
	if err != nil {
		t.Fatalf("read the embedded dist directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the embedded dist directory is empty")
	}
}

func TestTheBuiltApplicationIsEmbedded(t *testing.T) {
	t.Parallel()
	index, err := fs.ReadFile(Assets(), "dist/index.html")
	if err != nil {
		t.Skip("the dashboard is not built in this checkout; run `make dashboard-build` to embed the application")
	}
	if !strings.Contains(string(index), "<div id=\"root\">") {
		t.Error("the embedded index.html is not the Vite application shell")
	}
}

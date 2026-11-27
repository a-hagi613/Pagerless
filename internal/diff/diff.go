// Package diff renders the unified diff of a proposal from the verified base
// content. The server derives the diff, so a model cannot supply a patch that
// disagrees with the base revision it claims to edit.
package diff

import (
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// File is one changed file.
type File struct {
	Path    string `json:"path"`
	Diff    string `json:"diff"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// UnifiedDiff renders path from base to proposed.
func UnifiedDiff(path string, base, proposed []byte) (File, error) {
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(base)),
		B:        difflib.SplitLines(string(proposed)),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	})
	if err != nil {
		return File{}, err
	}
	file := File{Path: path, Diff: text}
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			file.Added++
		case strings.HasPrefix(line, "-"):
			file.Removed++
		}
	}
	return file, nil
}

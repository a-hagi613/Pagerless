package validation

import (
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// unifiedDiff renders the Diff from the base content to the proposed content.
// The server derives it, so a model-supplied patch cannot be inconsistent with
// the base commit.
func unifiedDiff(path string, base, proposed []byte) (FileDiff, error) {
	from := difflib.SplitLines(string(base))
	to := difflib.SplitLines(string(proposed))
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        from,
		B:        to,
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	})
	if err != nil {
		return FileDiff{}, err
	}
	diff := FileDiff{Path: path, Diff: text}
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			diff.Added++
		case strings.HasPrefix(line, "-"):
			diff.Removed++
		}
	}
	return diff, nil
}

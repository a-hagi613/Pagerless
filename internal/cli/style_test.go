package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestStylerStaysPlainWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	presentation := newPrinter(&out)
	presentation.section("Messages")
	presentation.field("Status", "completed")
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("a buffer received ANSI escapes: %q", out.String())
	}
	if !strings.Contains(out.String(), "Messages") || !strings.Contains(out.String(), "Status:") {
		t.Fatalf("the plain presentation lost its text: %q", out.String())
	}
}

func TestMarkdownLineStylesHeadingsBoldCodeAndBullets(t *testing.T) {
	presentation := &printer{style: styler{enabled: true}}
	line := presentation.markdownLine("- **Available=False** and `inventory-setting`")
	for _, want := range []string{"\x1b[36m•", "\x1b[1mAvailable=False", "\x1b[38;5;180minventory-setting"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q does not contain %q", line, want)
		}
	}
	heading := presentation.markdownLine("## Observed state")
	if !strings.Contains(heading, "\x1b[1;36mObserved state") {
		t.Fatalf("heading was not styled: %q", heading)
	}
	if strings.Contains(heading, "#") {
		t.Fatalf("heading marker survived: %q", heading)
	}
}

func TestDiffStylesHunksAdditionsAndRemovals(t *testing.T) {
	var out bytes.Buffer
	presentation := &printer{writer: &out, style: styler{enabled: true}}
	presentation.diff("--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new")
	text := out.String()
	for _, want := range []string{"\x1b[1m--- a/file", "\x1b[36m@@ -1 +1 @@", "\x1b[31m-old", "\x1b[32m+new"} {
		if !strings.Contains(text, want) {
			t.Fatalf("diff %q does not contain %q", text, want)
		}
	}
}

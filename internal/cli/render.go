package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// placeholder stands for a value that is absent from a record.
const placeholder = "none"

// maxInlineJSON bounds one JSON value in a human-readable line.
const maxInlineJSON = 400

// writeJSON writes one indented JSON document. Machine output never carries
// progress, which is written to standard error.
func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	return nil
}

// writeJSONLine writes one compact JSON value and a newline, used for a stream
// of events rather than one document.
func writeJSONLine(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	_, err = writer.Write(append(raw, '\n'))
	return err
}

// sanitize makes server-controlled text safe for a terminal. Newlines and tabs
// survive; other control characters and bidirectional overrides are escaped so
// a log line or repository document cannot move the cursor or spoof the
// surrounding text.
func sanitize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var safe strings.Builder
	safe.Grow(len(text))
	for _, char := range text {
		switch {
		case char == '\n' || char == '\t':
			safe.WriteRune(char)
		case char == '\r':
			safe.WriteString(`\x0d`)
		case char < 0x20 || char == 0x7f || (char >= 0x80 && char <= 0x9f):
			_, _ = fmt.Fprintf(&safe, `\x%02x`, char)
		case char >= '\u202a' && char <= '\u202e',
			char >= '\u2066' && char <= '\u2069':
			_, _ = fmt.Fprintf(&safe, `\u%04x`, char)
		default:
			safe.WriteRune(char)
		}
	}
	return safe.String()
}

// truncate shortens text to a byte bound without splitting a rune.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// compactJSON renders a raw value on one line, bounded for a progress line.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return truncate(sanitize(string(raw)), maxInlineJSON)
	}
	return truncate(sanitize(compact.String()), maxInlineJSON)
}

// indentJSON renders a raw value for a detail section, bounded like a line.
func indentJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return placeholder
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		return truncate(sanitize(string(raw)), maxResponseBytes)
	}
	return sanitize(truncate(indented.String(), maxResponseBytes))
}

// formatTime renders an RFC 3339 timestamp compactly in UTC.
func formatTime(value time.Time) string {
	if value.IsZero() {
		return placeholder
	}
	return value.UTC().Format("2006-01-02 15:04:05Z")
}

// formatAge renders how long ago a timestamp was, for a list row.
func formatAge(value time.Time) string {
	if value.IsZero() {
		return placeholder
	}
	age := max(time.Since(value), 0)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}

// formatDuration renders a completed tool's duration.
func formatDuration(milliseconds *int64) string {
	if milliseconds == nil {
		return placeholder
	}
	return fmt.Sprintf("%dms", *milliseconds)
}

// yesNo renders a boolean flag.
func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// activeTurn returns a printable active turn or the placeholder.
func activeTurn(id *string) string {
	if id == nil || *id == "" {
		return placeholder
	}
	return *id
}

// printer writes formatted text and keeps the first error, so a sequence of
// writes reports a failure once, at the end.
type printer struct {
	writer io.Writer
	err    error
}

// printf writes one formatted line unless an earlier write failed.
func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.writer, format, args...)
}

// field writes one labelled line of a presentation.
func (p *printer) field(label, value string) {
	p.printf("%-12s %s\n", label+":", value)
}

// section writes the heading of a presentation section, after a blank line.
func (p *printer) section(title string) {
	p.printf("\n%s\n", title)
}

// block writes every line of text with the indent in front of it. Empty text
// writes the placeholder.
func (p *printer) block(indent, text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		p.printf("%s%s\n", indent, placeholder)
		return
	}
	for line := range strings.SplitSeq(text, "\n") {
		p.printf("%s%s\n", indent, line)
	}
}

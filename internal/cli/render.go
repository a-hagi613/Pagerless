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
	style  styler
	err    error
}

// newPrinter builds a printer that colours only a terminal destination.
func newPrinter(writer io.Writer) *printer {
	return &printer{writer: writer, style: newStyler(writer)}
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
	p.printf("%s %s\n", p.style.dim(fmt.Sprintf("%-12s", label+":")), value)
}

// section writes the heading of a presentation section, after a blank line.
func (p *printer) section(title string) {
	p.printf("\n%s\n", p.style.heading(title))
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

// markdown renders the small, safe subset of Markdown that model answers use:
// headings, bullets, quotes, bold, inline code and links. It never interprets
// HTML, and it only changes presentation, never the text the line carries.
func (p *printer) markdown(indent, text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		p.printf("%s%s\n", indent, placeholder)
		return
	}
	for line := range strings.SplitSeq(text, "\n") {
		p.printf("%s%s\n", indent, p.markdownLine(line))
	}
}

// markdownLine styles one line of a model answer.
func (p *printer) markdownLine(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	lead := line[:len(line)-len(trimmed)]

	if heading, ok := markdownHeading(trimmed); ok {
		heading = strings.ReplaceAll(strings.ReplaceAll(heading, "`", ""), "**", "")
		return lead + p.style.heading(heading)
	}
	switch {
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "):
		return lead + p.style.cyan("•") + " " + p.inline(strings.TrimSpace(trimmed[2:]))
	case strings.HasPrefix(trimmed, "> "):
		return lead + p.style.dim("│ ") + p.inline(strings.TrimSpace(trimmed[2:]))
	}
	if number, rest, ok := markdownNumber(trimmed); ok {
		return lead + p.style.bold(number) + " " + p.inline(rest)
	}
	return lead + p.inline(trimmed)
}

// markdownHeading strips an ATX heading marker, if present.
func markdownHeading(line string) (string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[level:]), true
}

// markdownNumber splits an ordered-list marker ("12. text").
func markdownNumber(line string) (string, string, bool) {
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits+1 >= len(line) || line[digits] != '.' || line[digits+1] != ' ' {
		return "", "", false
	}
	return line[:digits+1], strings.TrimSpace(line[digits+2:]), true
}

// inline styles paired bold and inline-code spans. Unpaired markers are left
// alone, so a lone asterisk or backtick is still literal text.
func (p *printer) inline(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(text); {
		if strings.HasPrefix(text[index:], "**") {
			if end := strings.Index(text[index+2:], "**"); end >= 0 {
				out.WriteString(p.style.bold(text[index+2 : index+2+end]))
				index += end + 4
				continue
			}
		}
		if text[index] == '`' {
			if end := strings.IndexByte(text[index+1:], '`'); end >= 0 {
				out.WriteString(p.style.code(text[index+1 : index+1+end]))
				index += end + 2
				continue
			}
		}
		if text[index] == '[' {
			if label, address, end, ok := markdownLink(text[index:]); ok {
				out.WriteString(p.style.bold(label))
				out.WriteString(p.style.dim(" (" + address + ")"))
				index += end
				continue
			}
		}
		out.WriteByte(text[index])
		index++
	}
	return out.String()
}

// markdownLink parses "[label](address)" at the start of text.
func markdownLink(text string) (label, address string, consumed int, ok bool) {
	if !strings.HasPrefix(text, "[") {
		return "", "", 0, false
	}
	closeLabel := strings.IndexByte(text, ']')
	if closeLabel < 0 || closeLabel+1 >= len(text) || text[closeLabel+1] != '(' {
		return "", "", 0, false
	}
	closeAddress := strings.IndexByte(text[closeLabel+2:], ')')
	if closeAddress < 0 {
		return "", "", 0, false
	}
	label = text[1:closeLabel]
	address = text[closeLabel+2 : closeLabel+2+closeAddress]
	if label == "" || address == "" {
		return "", "", 0, false
	}
	return label, address, closeLabel + 2 + closeAddress + 1, true
}

// diff colours a unified diff: file headers bold, hunks cyan, additions green,
// removals red, and metadata dim.
func (p *printer) diff(text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		p.printf("%s\n", placeholder)
		return
	}
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			p.printf("%s\n", p.style.bold(line))
		case strings.HasPrefix(line, "@@"):
			p.printf("%s\n", p.style.cyan(line))
		case strings.HasPrefix(line, "+"):
			p.printf("%s\n", p.style.green(line))
		case strings.HasPrefix(line, "-"):
			p.printf("%s\n", p.style.red(line))
		case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "):
			p.printf("%s\n", p.style.dim(line))
		default:
			p.printf("%s\n", line)
		}
	}
}

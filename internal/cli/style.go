package cli

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// styler adds ANSI attributes only when the destination is a terminal. A pipe,
// a redirected file, a test buffer, NO_COLOR, or TERM=dumb gets the exact plain
// output the command has always produced, so machine consumers and golden
// tests are unaffected.
type styler struct {
	enabled bool
}

func newStyler(writer io.Writer) styler {
	return styler{enabled: colorEnabled(writer)}
}

func colorEnabled(writer io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

// wrap applies one SGR sequence and resets it afterwards.
func (s styler) wrap(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s styler) bold(text string) string { return s.wrap("1", text) }

// dim uses an explicit grey instead of SGR 2. Faint is widely ignored (pyte
// drops it), while the 256-colour grey keeps labels legible in every recorder.
func (s styler) dim(text string) string     { return s.wrap("38;5;245", text) }
func (s styler) red(text string) string     { return s.wrap("31", text) }
func (s styler) green(text string) string   { return s.wrap("32", text) }
func (s styler) yellow(text string) string  { return s.wrap("33", text) }
func (s styler) magenta(text string) string { return s.wrap("35", text) }
func (s styler) cyan(text string) string    { return s.wrap("36", text) }
func (s styler) heading(text string) string { return s.wrap("1;36", text) }

// code uses a muted warm tone so dense identifiers read as code without the
// glare of a saturated yellow.
func (s styler) code(text string) string { return s.wrap("38;5;180", text) }

// status paints a turn or tool status by meaning.
func (s styler) status(text string) string {
	switch strings.ToLower(text) {
	case "completed", "complete", "ok", "succeeded":
		return s.green(text)
	case "failed", "error":
		return s.red(text)
	case "stopped", "interrupted", "denied", "truncated", "timeout", "timed_out":
		return s.yellow(text)
	case "running":
		return s.cyan(text)
	default:
		return text
	}
}

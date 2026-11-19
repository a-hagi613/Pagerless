package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestTheParityMapOnlyNamesCommandsThatExist reads the Portal's CLI drawer
// manifest and asserts that every invocation it advertises resolves to a
// command and flags that this package registers. The manifest is TypeScript,
// so it is read as text; a parse failure fails the test rather than silently
// checking nothing.
func TestTheParityMapOnlyNamesCommandsThatExist(t *testing.T) {
	path := filepath.Join("..", "..", "dashboard", "src", "lib", "cli-parity.ts")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the parity map %s: %v", path, err)
	}
	invocations := parityInvocations(t, string(raw))
	if len(invocations) != 12 {
		t.Fatalf("parsed %d invocations from the parity map, want 12", len(invocations))
	}

	root := NewRootCommand()
	for _, invocation := range invocations {
		t.Run(invocation, func(t *testing.T) {
			assertInvocationExists(t, root, invocation)
		})
	}
}

// parityInvocations returns every invocation string in the parity map.
func parityInvocations(t *testing.T, source string) []string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "invocation:") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trimmed, "invocation:")), ",")
		if len(value) < 2 {
			t.Fatalf("malformed invocation in the parity map: %q", line)
		}
		quote := value[0]
		if quote != '"' && quote != '\'' {
			// The TypeScript interface declares `invocation: string;`.
			continue
		}
		if value[len(value)-1] != quote {
			t.Fatalf("unterminated invocation in the parity map: %q", line)
		}
		found = append(found, value[1:len(value)-1])
	}
	return found
}

// assertInvocationExists resolves one advertised invocation against the
// compiled command tree. Arguments are skipped; every command token and every
// flag must exist.
func assertInvocationExists(t *testing.T, root *cobra.Command, invocation string) {
	t.Helper()
	fields := strings.Fields(invocation)
	if len(fields) < 3 || fields[0] != "kubectl" || fields[1] != "page" {
		t.Fatalf("invocation %q does not start with \"kubectl page\"", invocation)
	}

	cmd := root
	rest := fields[2:]
	consumed := 0
	for _, token := range rest {
		if strings.HasPrefix(token, "-") {
			break
		}
		child := childCommand(cmd, token)
		if child == nil {
			break
		}
		cmd = child
		consumed++
	}
	if consumed == 0 {
		t.Fatalf("invocation %q names no subcommand under %q", invocation, cmd.CommandPath())
	}

	for _, token := range rest[consumed:] {
		if !strings.HasPrefix(token, "-") {
			continue
		}
		if !hasFlag(cmd, token) {
			t.Errorf("invocation %q uses %s, which %s does not register", invocation, token, cmd.CommandPath())
		}
	}
}

// childCommand returns the named subcommand, or nil when it is an argument.
func childCommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == name {
			return child
		}
	}
	return nil
}

// hasFlag reports whether the command or an ancestor registers the flag, by
// long name or by shorthand.
func hasFlag(cmd *cobra.Command, token string) bool {
	name := strings.TrimLeft(token, "-")
	if name == "" {
		return false
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	for c := cmd; c != nil; c = c.Parent() {
		if c.Flags().Lookup(name) != nil || c.PersistentFlags().Lookup(name) != nil {
			return true
		}
		// ShorthandLookup panics on anything but one ASCII character.
		if len(name) == 1 && (c.Flags().ShorthandLookup(name) != nil || c.PersistentFlags().ShorthandLookup(name) != nil) {
			return true
		}
	}
	return false
}

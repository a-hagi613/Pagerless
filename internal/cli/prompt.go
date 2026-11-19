package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// sourceFlags are the non-interactive ways to supply a credential. They exist
// so that no secret value ever has to be an argument.
type sourceFlags struct {
	// stdin reads KEY=value lines from standard input.
	stdin bool
	// file reads KEY=value lines from a file.
	file string
	// envFile reads KEY=value lines from an env file.
	envFile string
}

// register adds the source flags to a command.
func (s *sourceFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&s.stdin, "from-stdin", false, "read KEY=value lines from standard input")
	cmd.Flags().StringVar(&s.file, "from-file", "", "read KEY=value lines from a file")
	cmd.Flags().StringVar(&s.envFile, "from-env-file", "", "read KEY=value lines from an env file")
}

// chosen reports whether any non-interactive source was selected.
func (s sourceFlags) chosen() bool {
	return s.stdin || s.file != "" || s.envFile != ""
}

// validate refuses more than one source.
func (s sourceFlags) validate() error {
	chosen := 0
	for _, set := range []bool{s.stdin, s.file != "", s.envFile != ""} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		return usagef("choose one of --from-stdin, --from-file, or --from-env-file")
	}
	return nil
}

// secretValues returns the requested keys from the chosen source. When no
// source is given, it prompts for each key without echo when standard input is
// a terminal, and otherwise reads KEY=value lines from standard input.
func secretValues(cmd *cobra.Command, source sourceFlags, wanted []string) (map[string]string, error) {
	if err := source.validate(); err != nil {
		return nil, err
	}
	switch {
	case source.file != "":
		return readKeyValuesFile(source.file, wanted)
	case source.envFile != "":
		return readKeyValuesFile(source.envFile, wanted)
	case source.stdin:
		return readKeyValues(cmd.InOrStdin(), wanted)
	}
	in := cmd.InOrStdin()
	if stdinIsTerminal(in) {
		return promptSecrets(cmd, wanted)
	}
	return readKeyValues(in, wanted)
}

// readKeyValuesFile reads KEY=value lines from a file.
func readKeyValuesFile(path string, wanted []string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	return readKeyValues(file, wanted)
}

// readKeyValues parses KEY=value lines and keeps the wanted keys. Blank lines
// and comments are ignored, an optional `export ` prefix is accepted, and one
// pair of matching quotes is removed from a value.
func readKeyValues(in io.Reader, wanted []string) (map[string]string, error) {
	values := make(map[string]string, len(wanted))
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, usagef("expected KEY=value, got %q", line)
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))
		if !slices.Contains(wanted, key) {
			continue
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the credential input: %w", err)
	}
	return values, nil
}

// promptSecrets reads each wanted key from the terminal without echo.
func promptSecrets(cmd *cobra.Command, wanted []string) (map[string]string, error) {
	values := make(map[string]string, len(wanted))
	for _, key := range wanted {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", key)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return nil, fmt.Errorf("read %s without echo: %w", key, err)
		}
		values[key] = strings.TrimSpace(string(raw))
	}
	return values, nil
}

// promptVisible reads one non-secret line from the terminal, such as a base
// URL or a model name.
func promptVisible(cmd *cobra.Command, label string) (string, error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", label)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimSpace(line), nil
}

// stdinIsTerminal reports whether the input is an interactive terminal.
func stdinIsTerminal(in io.Reader) bool {
	file, ok := in.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// unquote removes one pair of matching single or double quotes.
func unquote(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

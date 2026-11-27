package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxTokenBytes bounds a token file so a wrong path cannot stream a disk image.
const maxTokenBytes = 8 << 10

// resolveToken reads the operator token from the file flag, the file variable,
// or the environment, in that order. The value is never echoed, logged, or put
// in a URL.
func (a *app) resolveToken() (string, error) {
	path := strings.TrimSpace(a.opts.tokenFile)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("PAGERLESS_TOKEN_FILE"))
	}
	if path != "" {
		return a.readToken(path)
	}
	if token := strings.TrimSpace(os.Getenv("PAGERLESS_TOKEN")); token != "" {
		return validToken(token)
	}
	return "", &ExitError{Code: ExitAuth, Err: errors.New(
		"no operator token: set PAGERLESS_TOKEN or pass --token-file (use - to read standard input)")}
}

// readToken reads one token from a file or from standard input.
func (a *app) readToken(path string) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(a.in(), maxTokenBytes+1))
	} else {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			_, _ = fmt.Fprintf(a.errOut(),
				"warning: %s is readable by other users; restrict it with chmod 600\n", sanitize(path))
		}
		raw, err = readTokenFile(path)
	}
	if err != nil {
		return "", &ExitError{Code: ExitAuth, Err: fmt.Errorf("read the operator token: %w", err)}
	}
	if len(raw) > maxTokenBytes {
		return "", &ExitError{Code: ExitAuth, Err: errors.New("the operator token is too long")}
	}
	return validToken(string(raw))
}

// readTokenFile reads at most one token file.
func readTokenFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
}

// validToken trims the outer whitespace of a single-line bearer token.
func validToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", &ExitError{Code: ExitAuth, Err: errors.New("the operator token is empty")}
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", &ExitError{Code: ExitAuth, Err: errors.New("the operator token must be one line")}
	}
	return token, nil
}

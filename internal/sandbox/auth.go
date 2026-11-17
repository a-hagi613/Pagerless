package sandbox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

// readEnvironment prevents inherited credential helpers and netrc files from providing unrelated credentials.
// The configured token is an environment-only header for this HTTPS repository path; redirects are refused.
func (r Repository) readEnvironment() ([]string, error) {
	config := [][2]string{
		{"credential.helper", ""},
		{"core.askPass", ""},
		{"http.extraHeader", ""},
		{"http.followRedirects", "false"},
	}
	if r.ReadToken != nil {
		u, err := readURL(r.URL)
		if err != nil {
			return nil, err
		}
		token, err := r.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("read the Sandbox token: %w", err)
		}
		if token == "" || strings.ContainsAny(token, "\x00\r\n\t ") {
			return nil, errors.New("the Sandbox read token must be nonempty and contain no spaces, tabs, CR, LF, or NUL")
		}
		credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		config = append(config, [2]string{
			"http." + strings.TrimSuffix(u.String(), "/") + "/.extraHeader", "Authorization: Basic " + credentials,
		})
	}
	env := []string{
		"HOME=" + os.DevNull, "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(config)),
	}
	for i, pair := range config {
		index := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+index+"="+pair[0], "GIT_CONFIG_VALUE_"+index+"="+pair[1])
	}
	return env, nil
}

func readURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Path == "" || path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") {
		return nil, errors.New("a Sandbox read token requires a clean HTTPS URL without credentials, query, or fragment")
	}
	return u, nil
}

// gitEnvironment keeps the process's trusted CA settings, but never inherits Git configuration, tracing,
// credential prompts, or repository-selection variables. Trusted callers add operation-specific Git settings.
func gitEnvironment() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if (strings.HasPrefix(strings.ToUpper(name), "GIT_") && name != "GIT_SSL_CAINFO" && name != "GIT_SSL_CAPATH") ||
			name == "SSH_ASKPASS" || name == "SSH_ASKPASS_REQUIRE" || name == "NETRC" ||
			name == "SSLKEYLOGFILE" || name == "USERPROFILE" {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

// redactGitOutput handles a Git server or proxy reflecting authentication in an error or unexpected answer.
func redactGitOutput(output string, env []string) string {
	for _, entry := range env {
		_, value, _ := strings.Cut(entry, "=")
		credentials, found := strings.CutPrefix(value, "Authorization: Basic ")
		if !found || credentials == "" {
			continue
		}
		output = strings.ReplaceAll(output, credentials, "[redacted]")
		decoded, err := base64.StdEncoding.DecodeString(credentials)
		if err != nil {
			continue
		}
		_, token, _ := strings.Cut(string(decoded), ":")
		if token != "" {
			output = strings.ReplaceAll(output, token, "[redacted]")
		}
	}
	return output
}

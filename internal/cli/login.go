package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/internal/dashboardauth"
)

// bootstrapTokenKey is the key the prompt and the env file use for the
// bootstrap token. It is not a Secret key: the token is exchanged once and
// never stored.
const bootstrapTokenKey = "BOOTSTRAP_TOKEN"

// newLoginCommand returns the command that exchanges the bootstrap token for a
// session and stores it for server mode.
func newLoginCommand(opts *options) *cobra.Command {
	var source sourceFlags
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Exchange the bootstrap token for a session that server mode reuses",
		Long: `Exchange the bootstrap token from pagerless-admin-secret for a signed session
cookie, and store the server URL and the session in the CLI configuration file
with mode 0600.

The token is never a flag. Supply it from an interactive prompt without echo,
from standard input, from a file, or from an env file under the key
BOOTSTRAP_TOKEN. A successful sign-in prints the command that deletes the
bootstrap Secret once the operator no longer needs it.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, opts, source)
		},
	}
	source.register(cmd)
	return cmd
}

// runLogin performs the token exchange and writes the local configuration.
func runLogin(cmd *cobra.Command, opts *options, source sourceFlags) error {
	if err := source.validate(); err != nil {
		return err
	}
	if opts.server == "" {
		stored, err := loadConfig(opts.configPath)
		if err != nil {
			return err
		}
		opts.server = stored.Server
	}
	if strings.TrimSpace(opts.server) == "" {
		return usagef("login needs --server <url>")
	}
	client, err := newAPIClient(opts.server, "")
	if err != nil {
		return err
	}
	tokens, err := secretValues(cmd, source, []string{bootstrapTokenKey})
	if err != nil {
		return err
	}
	token := strings.TrimSpace(tokens[bootstrapTokenKey])
	if token == "" {
		return usagef("%s was not provided: use an interactive prompt, standard input, a file, or an env file",
			bootstrapTokenKey)
	}
	session, expiresAt, err := client.exchange(cmd.Context(), token)
	if err != nil {
		return err
	}
	path, err := saveConfig(opts.configPath, configFile{Server: client.base, SessionToken: session})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "using server mode (%s)\n", client.base)
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"signed in to %s; the session expires at %s and is stored in %s\n", client.base, expiresAt, path)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"once you no longer need the bootstrap token, delete it: kubectl -n %s delete secret pagerless-admin-secret\n",
		opts.pageNamespace)
	return err
}

// exchange posts the bootstrap token and returns the session cookie.
func (c *apiClient) exchange(ctx context.Context, token string) (string, string, error) {
	body, err := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
	if err != nil {
		return "", "", fmt.Errorf("encode the login request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("build the login request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("sign in to %s: %w", c.base, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("read the login answer: %w", err)
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return "", "", usagef("token sign-in is disabled on %s: the server runs auth.mode=disabled", c.base)
	case http.StatusTooManyRequests:
		retryAfter := response.Header.Get("Retry-After")
		if retryAfter == "" {
			retryAfter = "a few minutes"
		} else if seconds, err := strconv.Atoi(retryAfter); err == nil {
			retryAfter = (time.Duration(seconds) * time.Second).String()
		}
		return "", "", fmt.Errorf("too many failed sign-ins from this address; retry in %s", retryAfter)
	case http.StatusUnauthorized:
		return "", "", fmt.Errorf("the bootstrap token was not accepted; read it again from Secret pagerless-admin-secret")
	default:
		return "", "", apiFailure(response.StatusCode, raw)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name != dashboardauth.DefaultCookieName {
			continue
		}
		var answer struct {
			ExpiresAt string `json:"expiresAt"`
		}
		_ = json.Unmarshal(raw, &answer)
		if answer.ExpiresAt == "" {
			answer.ExpiresAt = "the session's expiry"
		}
		return cookie.Value, answer.ExpiresAt, nil
	}
	return "", "", fmt.Errorf("the server answered without a session cookie")
}

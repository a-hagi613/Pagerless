package dashboardapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

const (
	// settingsTestTimeout bounds one connection test. A test runs while an operator waits, so it must
	// answer even when the endpoint is unreachable.
	settingsTestTimeout = 5 * time.Second
	// settingsTestBodyBytes bounds how much of a probe's body is read. The answer is a status, not a page.
	settingsTestBodyBytes = 4 << 10
	// defaultGitHubAPIURL is the public GitHub API. A Target Repository may override it for Enterprise.
	defaultGitHubAPIURL = "https://api.github.com"
)

// settingsTestRequest is the body of POST /api/settings/test.
type settingsTestRequest struct {
	// Target is one of "ai", "github", or "connector:<name>".
	Target string `json:"target"`
}

// settingsTestResponse is the outcome of one connection test.
type settingsTestResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// testSettings probes one configured dependency with the credentials the Secret already holds. It never
// writes anything and never returns a credential: the detail names the endpoint and the status only.
func (h *Handler) testSettings(w http.ResponseWriter, r *http.Request) {
	if h.requireCSRF(w, r) == nil {
		return
	}
	var request settingsTestRequest
	if !decodeWrite(w, r, &request) {
		return
	}
	if h.credentials == nil {
		writeAPIError(w, http.StatusNotImplemented, "settings_unavailable",
			"the server was started without a credentials store", nil)
		return
	}
	values, err := h.credentials.Read(r.Context())
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}

	target := strings.TrimSpace(request.Target)
	switch {
	case target == "ai":
		writeJSON(w, http.StatusOK, h.testAI(r.Context(), values))
	case target == "github":
		writeJSON(w, http.StatusOK, h.testGitHub(r.Context(), values))
	case strings.HasPrefix(target, "connector:"):
		writeJSON(w, http.StatusOK, testConnector(strings.TrimPrefix(target, "connector:"), values))
	default:
		writeAPIError(w, http.StatusBadRequest, "settings_test_target",
			`target must be "ai", "github", or "connector:<name>"`, nil)
	}
}

// testAI asks the OpenAI-compatible endpoint for its model list, which every implementation of the
// protocol exposes and which requires the API key.
func (h *Handler) testAI(ctx context.Context, values map[string]string) settingsTestResponse {
	base := strings.TrimSpace(values[credentials.KeyAIBaseURL])
	key := strings.TrimSpace(values[credentials.KeyAIAPIKey])
	if base == "" {
		return settingsTestResponse{Detail: credentials.KeyAIBaseURL + " is not configured"}
	}
	if key == "" {
		return settingsTestResponse{Detail: credentials.KeyAIAPIKey + " is not configured"}
	}
	endpoint := strings.TrimRight(base, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return settingsTestResponse{Detail: fmt.Sprintf("build the request for %s: %v", endpoint, err)}
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	return probe(request, endpoint)
}

// testGitHub reads the configured repository with the configured token, which proves both halves of the
// Target Repository at once.
func (h *Handler) testGitHub(ctx context.Context, values map[string]string) settingsTestResponse {
	repository := strings.TrimSpace(values[credentials.KeyGitHubRepo])
	token := strings.TrimSpace(values[credentials.KeyGitHubToken])
	if repository == "" {
		return settingsTestResponse{Detail: credentials.KeyGitHubRepo + " is not configured"}
	}
	if token == "" {
		return settingsTestResponse{Detail: credentials.KeyGitHubToken + " is not configured"}
	}
	base := strings.TrimSpace(values[credentials.KeyGitHubAPIURL])
	if base == "" {
		base = defaultGitHubAPIURL
	}
	endpoint := strings.TrimRight(base, "/") + "/repos/" + repository
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return settingsTestResponse{Detail: fmt.Sprintf("build the request for %s: %v", endpoint, err)}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return probe(request, endpoint)
}

// testConnector checks the one thing the credentials Secret owns for a Connector: the token the runtime
// will read. A Connector's type, endpoint, and enablement are deployment configuration, not credentials,
// so reachability is not probed from here.
func testConnector(name string, values map[string]string) settingsTestResponse {
	name = strings.TrimSpace(name)
	if name == "" {
		return settingsTestResponse{Detail: "a connector test needs a name"}
	}
	key := credentials.ConnectorTokenKey(name)
	token := strings.TrimSpace(values[key])
	if token == "" {
		return settingsTestResponse{Detail: key + " is not configured"}
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return settingsTestResponse{Detail: key + " contains whitespace; paste the token without padding"}
	}
	return settingsTestResponse{
		OK: true,
		Detail: fmt.Sprintf(
			"%s is stored and well-formed. The Connector's endpoint is deployment configuration, so this checks the credential, not reachability.",
			key),
	}
}

// probe runs one request and reports the status without reading more than a bounded body.
func probe(request *http.Request, endpoint string) settingsTestResponse {
	client := &http.Client{Timeout: settingsTestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return settingsTestResponse{Detail: fmt.Sprintf("%s: %v", endpoint, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, settingsTestBodyBytes))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return settingsTestResponse{
			OK:     true,
			Detail: fmt.Sprintf("%s answered %s", endpoint, response.Status),
		}
	}
	return settingsTestResponse{Detail: fmt.Sprintf("%s answered %s", endpoint, response.Status)}
}

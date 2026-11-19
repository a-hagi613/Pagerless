package dashboardapi

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

// SettingsResponse is the body of GET /api/settings and of a successful write.
// Every credential is a masked Field: the API never returns a stored value, not
// even truncated.
type SettingsResponse struct {
	credentials.View
	// AI, Repository, and Connectors are the Portal's grouped view of the same
	// masked fields, so the settings route renders one screen from one request.
	AI         AISettings          `json:"ai"`
	Repository RepositorySettings  `json:"repository"`
	Connectors []ConnectorSettings `json:"connectors"`
}

// AISettings is the model endpoint. The key is masked; the URL and model are not
// secret and are returned so the form can be prefilled.
type AISettings struct {
	BaseURL string            `json:"baseUrl"`
	Model   string            `json:"model"`
	APIKey  credentials.Field `json:"apiKey"`
}

// RepositorySettings is the Target Repository. The token is masked.
type RepositorySettings struct {
	Owner  string            `json:"owner"`
	Repo   string            `json:"repo"`
	Token  credentials.Field `json:"token"`
	APIURL string            `json:"apiUrl,omitempty"`
}

// ConnectorSettings is one connector's masked token.
type ConnectorSettings struct {
	Name  string            `json:"name"`
	Token credentials.Field `json:"token"`
}

// settingsPatch is the body of PATCH /api/settings. It accepts the flat key form
// that the CLI uses and the grouped form that the Portal form posts, so both
// surfaces write the same Secret keys.
type settingsPatch struct {
	Values     map[string]string `json:"values"`
	AI         *aiPatch          `json:"ai"`
	Repository *repositoryPatch  `json:"repository"`
	Connectors []connectorPatch  `json:"connectors"`
}

type aiPatch struct {
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
	APIKey  string `json:"apiKey"`
}

type repositoryPatch struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Token  string `json:"token"`
	APIURL string `json:"apiUrl"`
}

type connectorPatch struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// values flattens the patch into Secret keys, ignoring empty fields so that a
// form that leaves an unchanged credential blank does not clear it.
func (p settingsPatch) values() map[string]string {
	values := make(map[string]string, len(p.Values)+3)
	for key, value := range p.Values {
		if strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	if p.AI != nil {
		putIfSet(values, credentials.KeyAIBaseURL, p.AI.BaseURL)
		putIfSet(values, credentials.KeyAIModel, p.AI.Model)
		putIfSet(values, credentials.KeyAIAPIKey, p.AI.APIKey)
	}
	if p.Repository != nil {
		if p.Repository.Owner != "" || p.Repository.Repo != "" {
			putIfSet(values, credentials.KeyGitHubRepo, p.Repository.Owner+"/"+p.Repository.Repo)
		}
		putIfSet(values, credentials.KeyGitHubToken, p.Repository.Token)
		putIfSet(values, credentials.KeyGitHubAPIURL, p.Repository.APIURL)
	}
	for _, connector := range p.Connectors {
		if strings.TrimSpace(connector.Name) == "" {
			continue
		}
		putIfSet(values, credentials.ConnectorTokenKey(connector.Name), connector.Token)
	}
	return values
}

// putIfSet copies a non-empty value into the flat key map.
func putIfSet(values map[string]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	values[key] = value
}

// settings answers with every documented credential, masked.
func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if h.requireUser(w, r) == nil {
		return
	}
	if h.credentials == nil {
		writeAPIError(w, http.StatusNotImplemented, "settings_unavailable",
			"the server was started without a credentials store", nil)
		return
	}
	view, err := h.credentials.View(r.Context())
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	values, err := h.credentials.Read(r.Context())
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, settingsResponse(view, values))
}

// updateSettings merges submitted keys into the credentials Secret. A key that
// is not submitted is never cleared, so two surfaces can write different
// sections without overwriting each other.
func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	if h.requireCSRF(w, r) == nil {
		return
	}
	var patch settingsPatch
	if !decodeWrite(w, r, &patch) {
		return
	}
	if h.credentials == nil {
		writeAPIError(w, http.StatusNotImplemented, "settings_unavailable",
			"the server was started without a credentials store", nil)
		return
	}
	values := patch.values()
	if len(values) == 0 {
		writeAPIError(w, http.StatusBadRequest, "settings_empty", "no credential was submitted", nil)
		return
	}
	view, err := h.credentials.Write(r.Context(), values)
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	merged, err := h.credentials.Read(r.Context())
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, settingsResponse(view, merged))
}

// unsetSetting clears one key without touching the others.
func (h *Handler) unsetSetting(w http.ResponseWriter, r *http.Request) {
	if h.requireCSRF(w, r) == nil {
		return
	}
	key := strings.ToUpper(strings.TrimSpace(r.PathValue("key")))
	if !credentials.SecretKey(key) {
		writeAPIError(w, http.StatusBadRequest, "settings_unknown_key",
			"unknown credential key: accepted keys are fixed upper-snake names and CONNECTOR_<NAME>_TOKEN", nil)
		return
	}
	if h.credentials == nil {
		writeAPIError(w, http.StatusNotImplemented, "settings_unavailable",
			"the server was started without a credentials store", nil)
		return
	}
	view, err := h.credentials.Clear(r.Context(), []string{key})
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	values, err := h.credentials.Read(r.Context())
	if err != nil {
		h.writeSettingsError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, settingsResponse(view, values))
}

// settingsResponse groups the flat view for the Portal form.
func settingsResponse(view credentials.View, values map[string]string) SettingsResponse {
	repository := values[credentials.KeyGitHubRepo]
	owner, repo, _ := strings.Cut(repository, "/")
	view.Values = nonSecretValues(values)
	return SettingsResponse{
		View: view,
		AI: AISettings{
			BaseURL: values[credentials.KeyAIBaseURL],
			Model:   values[credentials.KeyAIModel],
			APIKey:  view.Fields[credentials.KeyAIAPIKey],
		},
		Repository: RepositorySettings{
			Owner:  owner,
			Repo:   repo,
			Token:  view.Fields[credentials.KeyGitHubToken],
			APIURL: values[credentials.KeyGitHubAPIURL],
		},
		Connectors: connectorSettings(view),
	}
}

// connectorSettings lists the connector tokens the Secret carries.
func connectorSettings(view credentials.View) []ConnectorSettings {
	connectors := make([]ConnectorSettings, 0)
	for key, field := range view.Fields {
		if !credentials.IsConnectorTokenKey(key) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, credentials.ConnectorTokenPrefix), credentials.ConnectorTokenSuffix)
		connectors = append(connectors, ConnectorSettings{Name: strings.ToLower(name), Token: field})
	}
	sortConnectors(connectors)
	if connectors == nil {
		connectors = []ConnectorSettings{}
	}
	return connectors
}

// nonSecretValues keeps the configuration values and drops every credential.
func nonSecretValues(values map[string]string) map[string]string {
	kept := make(map[string]string, len(credentials.NonSecretKeys()))
	for key, value := range values {
		if credentials.NonSecret(key) && strings.TrimSpace(value) != "" {
			kept[key] = value
		}
	}
	return kept
}

// sortConnectors orders the connector list by name, so the response is stable.
func sortConnectors(connectors []ConnectorSettings) {
	slices.SortFunc(connectors, func(a, b ConnectorSettings) int {
		return strings.Compare(a.Name, b.Name)
	})
}

// writeSettingsError maps a credentials failure to an HTTP answer. An
// externally managed Secret and a missing Secret are both refusals with a
// machine-readable code, never a create.
func (h *Handler) writeSettingsError(w http.ResponseWriter, r *http.Request, err error) {
	if external, ok := errors.AsType[*credentials.ExternalError](err); ok {
		writeAPIError(w, http.StatusConflict, "settings_externally_managed", external.Error(),
			map[string]any{"manager": external.Manager})
		return
	}
	if invalid, ok := errors.AsType[*credentials.InvalidError](err); ok {
		writeAPIError(w, http.StatusBadRequest, "settings_invalid", invalid.Error(), nil)
		return
	}
	if missing, ok := errors.AsType[*credentials.MissingError](err); ok {
		writeAPIError(w, http.StatusFailedDependency, "credentials_secret_missing", missing.Error(),
			map[string]any{"secret": missing.Secret})
		return
	}
	h.internal(w, r.Context(), err)
}

// writeAPIError writes a machine-readable error body. extra adds the fields a
// caller needs to act, such as the external manager's name.
func writeAPIError(w http.ResponseWriter, status int, code, message string, extra map[string]any) {
	body := map[string]any{"code": code, "message": message}
	maps.Copy(body, extra)
	writeJSON(w, status, map[string]any{"error": body})
}

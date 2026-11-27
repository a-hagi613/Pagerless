// Package grafanamcp exposes the read-only subset of the Grafana HTTP API that
// an investigation needs: unified alerts, dashboard search, one dashboard and
// the data source inventory. Data source secrets are stripped before the result
// leaves the process.
package grafanamcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

const maxResponseBytes = 512 << 10

var uidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Grafana is one read-only Grafana endpoint.
type Grafana struct {
	baseURL *url.URL
	token   func(context.Context) (string, error)
	client  *http.Client
}

// New validates the endpoint and returns the provider.
func New(baseURL string, token func(context.Context) (string, error), client *http.Client) (*Grafana, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("grafana: the URL must be an absolute http or https URL")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Grafana{baseURL: parsed, token: token, client: client}, nil
}

// Tools returns the admitted Grafana tools.
func (g *Grafana) Tools() []mcpserver.Tool {
	return []mcpserver.Tool{
		{
			Name:        "grafana_alerts",
			Description: "List Grafana unified-alerting alerts and their labels, state and annotations.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Call:        g.alerts,
		},
		{
			Name:        "grafana_dashboards",
			Description: "Search Grafana dashboards by title, tag or folder.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string"},
					"tag":{"type":"string"},
					"folder":{"type":"string"}
				},
				"additionalProperties":false
			}`),
			Call: g.dashboards,
		},
		{
			Name:        "grafana_dashboard",
			Description: "Read one dashboard by its UID, including its panels and template variables.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{"uid":{"type":"string"}},
				"required":["uid"],
				"additionalProperties":false
			}`),
			Call: g.dashboard,
		},
		{
			Name:        "grafana_datasources",
			Description: "List the configured Grafana data sources; credential fields are removed.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Call:        g.datasources,
		},
	}
}

func (g *Grafana) alerts(ctx context.Context, _ json.RawMessage) (string, error) {
	return g.get(ctx, "/api/alertmanager/grafana/api/v2/alerts", nil)
}

func (g *Grafana) dashboards(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments struct {
		Query  string `json:"query"`
		Tag    string `json:"tag"`
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", fmt.Errorf("grafana: the arguments are not a JSON object")
	}
	values := url.Values{"type": {"dash-db"}, "limit": {"50"}}
	if arguments.Query != "" {
		values.Set("query", arguments.Query)
	}
	if arguments.Tag != "" {
		values.Set("tag", arguments.Tag)
	}
	if arguments.Folder != "" {
		values.Set("folder", arguments.Folder)
	}
	return g.get(ctx, "/api/search", values)
}

func (g *Grafana) dashboard(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", fmt.Errorf("grafana: the arguments are not a JSON object")
	}
	if !uidPattern.MatchString(arguments.UID) {
		return "", fmt.Errorf("grafana: uid is not a valid dashboard UID")
	}
	return g.get(ctx, "/api/dashboards/uid/"+url.PathEscape(arguments.UID), nil)
}

func (g *Grafana) datasources(ctx context.Context, _ json.RawMessage) (string, error) {
	body, err := g.get(ctx, "/api/datasources", nil)
	if err != nil {
		return "", err
	}
	var sources []map[string]any
	if err := json.Unmarshal([]byte(body), &sources); err != nil {
		return "", fmt.Errorf("grafana: the data source list is not valid JSON")
	}
	for _, source := range sources {
		delete(source, "secureJsonData")
		delete(source, "basicAuthPassword")
		delete(source, "password")
		delete(source, "token")
	}
	clean, err := json.Marshal(sources)
	if err != nil {
		return "", fmt.Errorf("grafana: the data source list could not be encoded")
	}
	return string(clean), nil
}

func (g *Grafana) get(ctx context.Context, path string, query url.Values) (string, error) {
	endpoint := *g.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("grafana: build the request")
	}
	request.Header.Set("Accept", "application/json")
	if g.token != nil {
		token, tokenErr := g.token(ctx)
		if tokenErr != nil {
			return "", fmt.Errorf("grafana: read the credential")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := g.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("grafana: the request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("grafana: the response could not be read")
	}
	if len(body) > maxResponseBytes {
		return "", fmt.Errorf("grafana: the response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("grafana: HTTP %d: %s", response.StatusCode, bounded(string(body)))
	}
	return string(body), nil
}

func bounded(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

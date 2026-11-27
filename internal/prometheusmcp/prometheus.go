// Package prometheusmcp exposes the read-only subset of the Prometheus HTTP API
// that an investigation needs: instant queries, firing alerts and active
// targets. It is an MCP provider, not a scraper, and it never accepts a write.
package prometheusmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

// maxResponseBytes bounds one Prometheus response.
const maxResponseBytes = 512 << 10

// Prometheus is one read-only Prometheus endpoint.
type Prometheus struct {
	baseURL *url.URL
	token   func(context.Context) (string, error)
	client  *http.Client
}

// New validates the endpoint and returns the provider.
func New(baseURL string, token func(context.Context) (string, error), client *http.Client) (*Prometheus, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("prometheus: the URL must be an absolute http or https URL")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Prometheus{baseURL: parsed, token: token, client: client}, nil
}

// Tools returns the admitted Prometheus tools.
func (p *Prometheus) Tools() []mcpserver.Tool {
	return []mcpserver.Tool{
		{
			Name:        "prometheus_query",
			Description: "Run one instant PromQL query and return the raw Prometheus result.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"query":{"type":"string","description":"PromQL expression"},
					"time":{"type":"string","description":"Optional RFC3339 or Unix timestamp"}
				},
				"required":["query"],
				"additionalProperties":false
			}`),
			Call: p.query,
		},
		{
			Name:        "prometheus_alerts",
			Description: "List the alerts Prometheus currently reports, including their state and labels.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Call:        p.alerts,
		},
		{
			Name:        "prometheus_targets",
			Description: "List the active scrape targets and their health.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Call:        p.targets,
		},
	}
}

func (p *Prometheus) query(ctx context.Context, raw json.RawMessage) (string, error) {
	var arguments struct {
		Query string `json:"query"`
		Time  string `json:"time"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", fmt.Errorf("prometheus: the arguments are not a JSON object")
	}
	if strings.TrimSpace(arguments.Query) == "" {
		return "", fmt.Errorf("prometheus: query is required")
	}
	values := url.Values{"query": {arguments.Query}}
	if arguments.Time != "" {
		values.Set("time", arguments.Time)
	}
	return p.get(ctx, "/api/v1/query", values)
}

func (p *Prometheus) alerts(ctx context.Context, _ json.RawMessage) (string, error) {
	return p.get(ctx, "/api/v1/alerts", nil)
}

func (p *Prometheus) targets(ctx context.Context, _ json.RawMessage) (string, error) {
	return p.get(ctx, "/api/v1/targets", url.Values{"state": {"active"}})
}

func (p *Prometheus) get(ctx context.Context, path string, query url.Values) (string, error) {
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", fmt.Errorf("prometheus: build the request")
	}
	request.Header.Set("Accept", "application/json")
	if p.token != nil {
		token, tokenErr := p.token(ctx)
		if tokenErr != nil {
			return "", fmt.Errorf("prometheus: read the credential")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("prometheus: the request failed")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("prometheus: the response could not be read")
	}
	if len(body) > maxResponseBytes {
		return "", fmt.Errorf("prometheus: the response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("prometheus: HTTP %d: %s", response.StatusCode, bounded(string(body)))
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

package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Datadog queries the Datadog Logs Search API.
type Datadog struct {
	client      *base.Client
	query       *base.Template
	limit       int
	apiKey      string
	appKey      string
	cluster     string
	environment string
}

// NewDatadog builds the Datadog Logs Connector.
func NewDatadog(
	client *base.Client, query *base.Template, limit int, apiKey, appKey string,
	clusterName, environment string,
) *Datadog {
	return &Datadog{
		client: client, query: query, limit: limit, apiKey: apiKey, appKey: appKey,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (d *Datadog) Name() string { return "datadog" }

// datadogSearchRequest is the Logs Search body.
type datadogSearchRequest struct {
	Filter map[string]string `json:"filter"`
	Page   map[string]any    `json:"page"`
	Sort   string            `json:"sort"`
}

// QueryLogs reads at most the configured pages.
func (d *Datadog) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	rendered, err := d.query.Render(contract.LogFields(q, d.cluster, d.environment))
	if err != nil {
		return contract.LogPage{}, err
	}
	until := q.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := q.Since
	if since.IsZero() || !since.Before(until) {
		since = until.Add(-time.Hour)
	}
	limit := d.limit
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	headers := map[string]string{"DD-API-KEY": d.apiKey, "DD-APPLICATION-KEY": d.appKey}
	page := contract.LogPage{}
	cursor := ""
	for pages := 0; pages < d.client.MaxPages(); pages++ {
		request := datadogSearchRequest{
			Filter: map[string]string{
				"query": rendered,
				"from":  since.UTC().Format(time.RFC3339),
				"to":    until.UTC().Format(time.RFC3339),
			},
			Page: map[string]any{"limit": limit},
			Sort: "timestamp",
		}
		if cursor != "" {
			request.Page["cursor"] = cursor
		}
		body, err := json.Marshal(request)
		if err != nil {
			return page, err
		}
		response, err := d.client.Post(ctx, "/api/v2/logs/events/search", nil, body, "application/json", headers)
		if err != nil {
			return page, err
		}
		var decoded datadogSearchResponse
		if err := json.Unmarshal(response.Body, &decoded); err != nil {
			return page, fmt.Errorf("decode the Datadog logs response: %w", err)
		}
		for _, item := range decoded.Data {
			entry := contract.LogEntry{
				Timestamp: parseDatadogTime(item.Attributes.Timestamp),
				Line:      item.Attributes.Message,
				Severity:  item.Attributes.Status,
				Stream:    map[string]string{"service": item.Attributes.Service, "host": item.Attributes.Host},
			}
			for _, tag := range item.Attributes.Tags {
				key, value, found := strings.Cut(tag, ":")
				if found {
					entry.Stream[key] = value
				}
			}
			page.Entries = append(page.Entries, entry)
			page.Bytes += len(entry.Line)
			if len(page.Entries) >= limit {
				page.Truncated = true
				return page, nil
			}
		}
		page.Truncated = page.Truncated || response.Truncated
		cursor = decoded.Meta.Page.After
		if cursor == "" || len(decoded.Data) == 0 {
			break
		}
	}
	return page, nil
}

// datadogSearchResponse is the Logs Search envelope.
type datadogSearchResponse struct {
	Data []struct {
		Attributes struct {
			Timestamp string   `json:"timestamp"`
			Message   string   `json:"message"`
			Status    string   `json:"status"`
			Service   string   `json:"service"`
			Host      string   `json:"host"`
			Tags      []string `json:"tags"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		Page struct {
			After string `json:"after"`
		} `json:"page"`
	} `json:"meta"`
}

// parseDatadogTime parses Datadog's RFC3339 timestamp.
func parseDatadogTime(raw string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// assert the interface.
var _ contract.LogConnector = (*Datadog)(nil)

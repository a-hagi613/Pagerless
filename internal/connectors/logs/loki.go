package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// HeaderTenant is the Loki tenant header.
const HeaderTenant = "X-Scope-OrgID"

// Loki queries Grafana Loki's query_range API.
type Loki struct {
	client   *base.Client
	query    *base.Template
	tenantID string
	limit    int
}

// NewLoki builds the Loki Connector.
func NewLoki(client *base.Client, query *base.Template, tenantID string, limit int) *Loki {
	return &Loki{client: client, query: query, tenantID: tenantID, limit: limit}
}

// Name returns the connector name.
func (l *Loki) Name() string { return "loki" }

// lokiResponse is the Loki query_range envelope.
type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// QueryLogs reads at most the configured pages, each page bounded by limit and
// the response byte cap.
func (l *Loki) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	page := contract.LogPage{}
	until := q.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := q.Since
	if since.IsZero() || !since.Before(until) {
		since = until.Add(-time.Hour)
	}
	remaining := l.limit
	if q.Limit > 0 && q.Limit < remaining {
		remaining = q.Limit
	}
	fields := contract.LogFields(q, "", "")
	for pages := 0; pages < l.client.MaxPages() && remaining > 0; pages++ {
		rendered, err := l.query.Render(fields)
		if err != nil {
			return page, err
		}
		values := url.Values{}
		values.Set("query", rendered)
		values.Set("start", strconv.FormatInt(since.UnixNano(), 10))
		values.Set("end", strconv.FormatInt(until.UnixNano(), 10))
		values.Set("limit", strconv.Itoa(remaining))
		values.Set("direction", "backward")
		headers := map[string]string{}
		if l.tenantID != "" {
			headers[HeaderTenant] = l.tenantID
		}
		response, err := l.client.GetWithHeaders(ctx, "/loki/api/v1/query_range", values, headers)
		if err != nil {
			return page, err
		}
		var decoded lokiResponse
		if err := json.Unmarshal(response.Body, &decoded); err != nil {
			return page, fmt.Errorf("decode the Loki response: %w", err)
		}
		count := 0
		oldest := time.Time{}
		for _, stream := range decoded.Data.Result {
			for _, value := range stream.Values {
				nanoseconds, err := strconv.ParseInt(value[0], 10, 64)
				if err != nil {
					continue
				}
				stamp := time.Unix(0, nanoseconds).UTC()
				if oldest.IsZero() || stamp.Before(oldest) {
					oldest = stamp
				}
				page.Entries = append(page.Entries, contract.LogEntry{
					Timestamp: stamp, Stream: stream.Stream, Line: value[1],
				})
				count++
			}
		}
		page.Truncated = page.Truncated || response.Truncated
		if count == 0 || remaining <= count || oldest.IsZero() {
			break
		}
		remaining -= count
		until = oldest.Add(-time.Nanosecond)
	}
	if len(page.Entries) >= l.limit {
		page.Truncated = true
	}
	for _, entry := range page.Entries {
		page.Bytes += len(entry.Line)
	}
	return page, nil
}

// assert the interface.
var _ contract.LogConnector = (*Loki)(nil)

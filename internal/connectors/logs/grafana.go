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
	"github.com/a-hagi613/pagerless/internal/connectors/grafana"
)

// Grafana queries a datasource through Grafana, either through the datasource
// proxy or through the unified query API.
type Grafana struct {
	client      *base.Client
	mode        string
	datasource  string
	query       *base.Template
	limit       int
	cluster     string
	environment string
}

// NewGrafana builds the Grafana log Connector.
func NewGrafana(
	client *base.Client, mode, datasourceUID string, query *base.Template, limit int,
	clusterName, environment string,
) *Grafana {
	return &Grafana{
		client: client, mode: mode, datasource: datasourceUID, query: query, limit: limit,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (g *Grafana) Name() string { return "grafana" }

// QueryLogs reads through the configured Grafana mode.
func (g *Grafana) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	rendered, err := g.query.Render(contract.LogFields(q, g.cluster, g.environment))
	if err != nil {
		return contract.LogPage{}, err
	}
	limit := g.limit
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	if g.mode == "dsquery" {
		return g.queryUnified(ctx, q, rendered, limit)
	}
	values := url.Values{}
	values.Set("query", rendered)
	values.Set("start", strconv.FormatInt(q.Since.UnixNano(), 10))
	values.Set("end", strconv.FormatInt(q.Until.UnixNano(), 10))
	values.Set("limit", strconv.Itoa(limit))
	values.Set("direction", "backward")
	response, err := g.client.Get(ctx,
		"/api/datasources/proxy/uid/"+url.PathEscape(g.datasource)+"/loki/api/v1/query_range", values)
	if err != nil {
		return contract.LogPage{}, err
	}
	return decodeLokiPage(response.Body, response.Truncated, limit)
}

// queryUnified calls Grafana's unified query API and converts the returned
// frames into log entries.
func (g *Grafana) queryUnified(
	ctx context.Context, q contract.Query, rendered string, limit int,
) (contract.LogPage, error) {
	body, err := grafana.QueryBody(g.datasource, rendered, map[string]any{
		"queryType": "range", "maxDataPoints": limit,
	}, q.Since, q.Until)
	if err != nil {
		return contract.LogPage{}, err
	}
	response, err := g.client.Post(ctx, "/api/ds/query", nil, body, "application/json", nil)
	if err != nil {
		return contract.LogPage{}, err
	}
	return decodeGrafanaLogFrames(response.Body, response.Truncated, limit)
}

// lokiResponse is the Loki envelope that Grafana's proxy preserves.
type lokiEnvelope struct {
	Data struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// decodeLokiPage converts a Loki response into a bounded page.
func decodeLokiPage(raw []byte, truncated bool, limit int) (contract.LogPage, error) {
	var decoded lokiEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return contract.LogPage{}, fmt.Errorf("decode the Grafana Loki proxy response: %w", err)
	}
	page := contract.LogPage{Truncated: truncated}
	for _, stream := range decoded.Data.Result {
		for _, value := range stream.Values {
			nanoseconds, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				continue
			}
			page.Entries = append(page.Entries, contract.LogEntry{
				Timestamp: time.Unix(0, nanoseconds).UTC(), Stream: stream.Stream, Line: value[1],
			})
			page.Bytes += len(value[1])
			if len(page.Entries) >= limit {
				page.Truncated = true
				return page, nil
			}
		}
	}
	return page, nil
}

// decodeGrafanaLogFrames converts unified-query frames into log entries.
func decodeGrafanaLogFrames(raw []byte, truncated bool, limit int) (contract.LogPage, error) {
	frames, err := grafana.Decode(raw)
	if err != nil {
		return contract.LogPage{}, err
	}
	page := contract.LogPage{Truncated: truncated}
	for _, frame := range frames {
		timeIndex := frame.Index("time", "timestamp")
		lineIndex := frame.Index("line", "body", "message", "log")
		if lineIndex < 0 {
			continue
		}
		for row := 0; row < frame.RowCount(); row++ {
			line := grafana.String(frame.Value(lineIndex, row))
			entry := contract.LogEntry{Line: line, Stream: map[string]string{"datasource": "grafana"}}
			if timeIndex >= 0 {
				entry.Timestamp = grafana.Time(frame.Value(timeIndex, row))
			}
			page.Entries = append(page.Entries, entry)
			page.Bytes += len(line)
			if len(page.Entries) >= limit {
				page.Truncated = true
				return page, nil
			}
		}
	}
	return page, nil
}

// assert the interface.
var _ contract.LogConnector = (*Grafana)(nil)

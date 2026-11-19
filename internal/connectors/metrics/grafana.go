package metrics

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

// Grafana queries a metrics datasource through Grafana, either through the
// datasource proxy or through the unified query API.
type Grafana struct {
	client      *base.Client
	mode        string
	datasource  string
	query       *base.Template
	step        time.Duration
	limit       int
	cluster     string
	environment string
}

// NewGrafana builds the Grafana metrics Connector.
func NewGrafana(
	client *base.Client, mode, datasourceUID string, query *base.Template, step time.Duration, limit int,
	clusterName, environment string,
) *Grafana {
	return &Grafana{
		client: client, mode: mode, datasource: datasourceUID, query: query, step: step, limit: limit,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (g *Grafana) Name() string { return "grafana" }

// QueryMetrics reads through the configured Grafana mode.
func (g *Grafana) QueryMetrics(ctx context.Context, q contract.MetricQuery) ([]contract.MetricSeries, error) {
	rendered, err := g.query.Render(contract.MetricFields(q, g.cluster, g.environment))
	if err != nil {
		return nil, err
	}
	if g.mode == "dsquery" {
		return g.queryUnified(ctx, q, rendered)
	}
	values := url.Values{}
	values.Set("query", rendered)
	values.Set("start", strconv.FormatInt(q.Since.Unix(), 10))
	values.Set("end", strconv.FormatInt(q.Until.Unix(), 10))
	step := g.step
	if q.Step > 0 {
		step = q.Step
	}
	values.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	response, err := g.client.Get(ctx,
		"/api/datasources/proxy/uid/"+url.PathEscape(g.datasource)+"/api/v1/query_range", values)
	if err != nil {
		return nil, err
	}
	return decodePrometheusSeries(response.Body, g.limit)
}

// queryUnified calls Grafana's unified query API and converts frames to series.
func (g *Grafana) queryUnified(
	ctx context.Context, q contract.MetricQuery, rendered string,
) ([]contract.MetricSeries, error) {
	body, err := grafana.QueryBody(g.datasource, rendered, map[string]any{
		"queryType": "range", "maxDataPoints": g.limit, "intervalMs": g.step.Milliseconds(),
	}, q.Since, q.Until)
	if err != nil {
		return nil, err
	}
	response, err := g.client.Post(ctx, "/api/ds/query", nil, body, "application/json", nil)
	if err != nil {
		return nil, err
	}
	frames, err := grafana.Decode(response.Body)
	if err != nil {
		return nil, err
	}
	series := make([]contract.MetricSeries, 0, len(frames))
	for _, frame := range frames {
		timeIndex := frame.Index("time", "timestamp")
		if timeIndex < 0 {
			continue
		}
		for fieldIndex, field := range frame.Fields {
			if fieldIndex == timeIndex {
				continue
			}
			parsed := contract.MetricSeries{Labels: field.Labels}
			for row := 0; row < frame.RowCount(); row++ {
				value, ok := grafana.Number(frame.Value(fieldIndex, row))
				if !ok {
					continue
				}
				parsed.Points = append(parsed.Points, contract.MetricPoint{
					Timestamp: grafana.Time(frame.Value(timeIndex, row)), Value: value,
				})
				if g.limit > 0 && len(parsed.Points) >= g.limit {
					break
				}
			}
			if len(parsed.Points) > 0 {
				series = append(series, parsed)
			}
		}
	}
	return series, nil
}

// decodePrometheusSeries parses the Prometheus envelope that Grafana's proxy
// preserves.
func decodePrometheusSeries(raw []byte, limit int) ([]contract.MetricSeries, error) {
	var decoded struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode the Grafana Prometheus proxy response: %w", err)
	}
	series := make([]contract.MetricSeries, 0, len(decoded.Data.Result))
	for _, result := range decoded.Data.Result {
		parsed := contract.MetricSeries{Labels: result.Metric}
		for _, value := range result.Values {
			point, ok := prometheusPoint(value)
			if !ok {
				continue
			}
			parsed.Points = append(parsed.Points, point)
			if limit > 0 && len(parsed.Points) >= limit {
				break
			}
		}
		series = append(series, parsed)
	}
	return series, nil
}

// assert the interface.
var _ contract.MetricsConnector = (*Grafana)(nil)

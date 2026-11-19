// Package metrics holds the read-only metrics Connectors. Every Connector
// implements contract.MetricsConnector, whose only method queries series.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Prometheus queries a Prometheus-compatible query_range API.
type Prometheus struct {
	client      *base.Client
	queries     map[string]*base.Template
	names       []string
	step        time.Duration
	maxSamples  int
	cluster     string
	environment string
}

// NewPrometheus builds the Prometheus Connector.
func NewPrometheus(
	client *base.Client, queries map[string]*base.Template, step time.Duration, maxSamples int,
	clusterName, environment string,
) *Prometheus {
	names := make([]string, 0, len(queries))
	for name := range queries {
		names = append(names, name)
	}
	slices.Sort(names)
	return &Prometheus{
		client: client, queries: queries, names: names, step: step, maxSamples: maxSamples,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (p *Prometheus) Name() string { return "prometheus" }

// prometheusResponse is the query_range envelope.
type prometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// QueryMetrics runs the named query, or every configured query when the caller
// does not name one.
func (p *Prometheus) QueryMetrics(ctx context.Context, q contract.MetricQuery) ([]contract.MetricSeries, error) {
	templates := p.selected(q.Query)
	series := make([]contract.MetricSeries, 0, 8)
	for _, template := range templates {
		rendered, err := template.Render(contract.MetricFields(q, p.cluster, p.environment))
		if err != nil {
			return series, err
		}
		values := url.Values{}
		values.Set("query", rendered)
		values.Set("start", strconv.FormatInt(q.Since.Unix(), 10))
		values.Set("end", strconv.FormatInt(q.Until.Unix(), 10))
		step := p.step
		if q.Step > 0 {
			step = q.Step
		}
		values.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
		response, err := p.client.Get(ctx, "/api/v1/query_range", values)
		if err != nil {
			return series, err
		}
		var decoded prometheusResponse
		if err := json.Unmarshal(response.Body, &decoded); err != nil {
			return series, fmt.Errorf("decode the Prometheus response: %w", err)
		}
		for _, result := range decoded.Data.Result {
			parsed := contract.MetricSeries{Labels: result.Metric}
			if decoded.Data.ResultType == "vector" || len(result.Values) == 0 {
				if point, ok := prometheusPoint(result.Value); ok {
					parsed.Points = append(parsed.Points, point)
				}
			} else {
				for _, value := range result.Values {
					if point, ok := prometheusPoint(value); ok {
						parsed.Points = append(parsed.Points, point)
					}
					if p.maxSamples > 0 && len(parsed.Points) >= p.maxSamples {
						break
					}
				}
			}
			series = append(series, parsed)
		}
	}
	return series, nil
}

// selected returns the templates a query names, or all of them.
func (p *Prometheus) selected(query string) []*base.Template {
	if query != "" {
		return []*base.Template{templateOf(query)}
	}
	templates := make([]*base.Template, 0, len(p.names))
	for _, name := range p.names {
		templates = append(templates, p.queries[name])
	}
	return templates
}

// templateOf wraps an already rendered query as a literal template.
func templateOf(query string) *base.Template {
	template, _ := base.CompileTemplate("query", query, false)
	return template
}

// prometheusPoint parses one [timestamp, value] pair.
func prometheusPoint(raw [2]any) (contract.MetricPoint, bool) {
	stamp, ok := number(raw[0])
	if !ok {
		return contract.MetricPoint{}, false
	}
	valueText, ok := raw[1].(string)
	if !ok {
		if value, ok := number(raw[1]); ok {
			return contract.MetricPoint{Timestamp: time.Unix(int64(stamp), 0).UTC(), Value: value}, true
		}
		return contract.MetricPoint{}, false
	}
	value, err := strconv.ParseFloat(valueText, 64)
	if err != nil {
		return contract.MetricPoint{}, false
	}
	return contract.MetricPoint{Timestamp: time.Unix(int64(stamp), 0).UTC(), Value: value}, true
}

// number converts a JSON number to a float64.
func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// assert the interface.
var _ contract.MetricsConnector = (*Prometheus)(nil)

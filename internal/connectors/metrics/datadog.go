package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Datadog queries the Datadog metrics query API.
type Datadog struct {
	client      *base.Client
	queries     map[string]*base.Template
	names       []string
	apiKey      string
	appKey      string
	cluster     string
	environment string
}

// NewDatadog builds the Datadog metrics Connector.
func NewDatadog(
	client *base.Client, queries map[string]*base.Template, apiKey, appKey string,
	clusterName, environment string,
) *Datadog {
	names := make([]string, 0, len(queries))
	for name := range queries {
		names = append(names, name)
	}
	slices.Sort(names)
	return &Datadog{
		client: client, queries: queries, names: names, apiKey: apiKey, appKey: appKey,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (d *Datadog) Name() string { return "datadog" }

// datadogMetricsResponse is the v1 query envelope.
type datadogMetricsResponse struct {
	Series []struct {
		Metric    string   `json:"metric"`
		TagSet    []string `json:"tag_set"`
		PointList [][2]any `json:"pointlist"`
	} `json:"series"`
}

// QueryMetrics runs the named query, or every configured query when the caller
// does not name one.
func (d *Datadog) QueryMetrics(ctx context.Context, q contract.MetricQuery) ([]contract.MetricSeries, error) {
	templates := d.queries
	if q.Query != "" {
		templates = map[string]*base.Template{queryName: templateOf(q.Query)}
	}
	names := d.names
	if q.Query != "" {
		names = []string{queryName}
	}
	series := make([]contract.MetricSeries, 0, 8)
	headers := map[string]string{"DD-API-KEY": d.apiKey, "DD-APPLICATION-KEY": d.appKey}
	for _, name := range names {
		rendered, err := templates[name].Render(contract.MetricFields(q, d.cluster, d.environment))
		if err != nil {
			return series, err
		}
		values := url.Values{}
		values.Set("from", strconv.FormatInt(q.Since.Unix(), 10))
		values.Set("to", strconv.FormatInt(q.Until.Unix(), 10))
		values.Set(queryName, rendered)
		response, err := d.client.GetWithHeaders(ctx, "/api/v1/query", values, headers)
		if err != nil {
			return series, err
		}
		var decoded datadogMetricsResponse
		if err := json.Unmarshal(response.Body, &decoded); err != nil {
			return series, fmt.Errorf("decode the Datadog metrics response: %w", err)
		}
		for _, result := range decoded.Series {
			parsed := contract.MetricSeries{Labels: map[string]string{"metric": result.Metric}}
			for _, tag := range result.TagSet {
				key, value, found := strings.Cut(tag, ":")
				if found {
					parsed.Labels[key] = value
				}
			}
			for _, point := range result.PointList {
				stamp, ok := number(point[0])
				if !ok {
					continue
				}
				value, ok := number(point[1])
				if !ok {
					continue
				}
				parsed.Points = append(parsed.Points, contract.MetricPoint{
					Timestamp: time.UnixMilli(int64(stamp)).UTC(), Value: value,
				})
			}
			series = append(series, parsed)
		}
	}
	return series, nil
}

// assert the interface.
var _ contract.MetricsConnector = (*Datadog)(nil)

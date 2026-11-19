package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/aws"
	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// queryName is the single query a CloudWatch metric adapter runs.
const queryName = "query"

// CloudWatch queries AWS CloudWatch metrics with SigV4 signing.
type CloudWatch struct {
	client      *base.Client
	caller      *aws.Caller
	queries     map[string]*base.Template
	names       []string
	namespace   string
	cluster     string
	environment string
}

// NewCloudWatch builds the CloudWatch metrics Connector.
func NewCloudWatch(
	client *base.Client, caller *aws.Caller, namespace string, queries map[string]*base.Template,
	clusterName, environment string,
) *CloudWatch {
	names := make([]string, 0, len(queries))
	for name := range queries {
		names = append(names, name)
	}
	slices.Sort(names)
	return &CloudWatch{
		client: client, caller: caller, queries: queries, names: names, namespace: namespace,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (c *CloudWatch) Name() string { return "cloudwatch" }

// cloudWatchQuery is one configured metric query.
type cloudWatchQuery struct {
	Metric     string            `json:"metric"`
	Namespace  string            `json:"namespace,omitempty"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	Stat       string            `json:"stat,omitempty"`
	Period     int               `json:"period,omitempty"`
}

// QueryMetrics runs the named query, or every configured query when the caller
// does not name one.
func (c *CloudWatch) QueryMetrics(ctx context.Context, q contract.MetricQuery) ([]contract.MetricSeries, error) {
	series := make([]contract.MetricSeries, 0, 8)
	for _, name := range c.selected(q.Query) {
		template := c.queries[name]
		if q.Query != "" {
			template = templateOf(q.Query)
		}
		rendered, err := template.Render(contract.MetricFields(q, c.cluster, c.environment))
		if err != nil {
			return series, err
		}
		var parsed cloudWatchQuery
		if err := json.Unmarshal([]byte(rendered), &parsed); err != nil {
			return series, fmt.Errorf("parse the CloudWatch metric query %s: %w", name, err)
		}
		namespace := firstNonEmpty(parsed.Namespace, c.namespace)
		if parsed.Metric == "" || namespace == "" {
			return series, fmt.Errorf("CloudWatch metric query %s needs a metric and a namespace", name)
		}
		stat := firstNonEmpty(parsed.Stat, "Average")
		period := parsed.Period
		if period <= 0 {
			period = 60
		}
		dimensions := make([]map[string]string, 0, len(parsed.Dimensions))
		dimensionNames := make([]string, 0, len(parsed.Dimensions))
		for dimension := range parsed.Dimensions {
			dimensionNames = append(dimensionNames, dimension)
		}
		slices.Sort(dimensionNames)
		for _, dimension := range dimensionNames {
			dimensions = append(dimensions, map[string]string{"Name": dimension, "Value": parsed.Dimensions[dimension]})
		}
		body, err := json.Marshal(map[string]any{
			"MetricDataQueries": []any{map[string]any{
				"Id": name,
				"MetricStat": map[string]any{
					"Metric": map[string]any{
						"Namespace": namespace, "MetricName": parsed.Metric, "Dimensions": dimensions,
					},
					"Period": period, "Stat": stat,
				},
				"ReturnData": true,
			}},
			"StartTime": q.Since.UTC().Format(time.RFC3339),
			"EndTime":   q.Until.UTC().Format(time.RFC3339),
			"ScanBy":    "TimestampDescending",
		})
		if err != nil {
			return series, err
		}
		response, err := c.caller.Call(ctx, "GraniteServiceVersion20100801.GetMetricData", body)
		if err != nil {
			return series, err
		}
		parsedSeries, err := decodeCloudWatchSeries(response.Body)
		if err != nil {
			return series, err
		}
		series = append(series, parsedSeries...)
	}
	return series, nil
}

// selected returns the query names a request selects.
func (c *CloudWatch) selected(query string) []string {
	if query != "" {
		return []string{queryName}
	}
	return c.names
}

// decodeCloudWatchSeries parses GetMetricData's results.
func decodeCloudWatchSeries(raw []byte) ([]contract.MetricSeries, error) {
	var decoded struct {
		MetricDataResults []struct {
			ID         string    `json:"Id"`
			Label      string    `json:"Label"`
			Timestamps []string  `json:"Timestamps"`
			Values     []float64 `json:"Values"`
		} `json:"MetricDataResults"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode the CloudWatch metrics response: %w", err)
	}
	series := make([]contract.MetricSeries, 0, len(decoded.MetricDataResults))
	for _, result := range decoded.MetricDataResults {
		parsed := contract.MetricSeries{Labels: map[string]string{"id": result.ID, "label": result.Label}}
		for index, stamp := range result.Timestamps {
			parsedAt, err := time.Parse(time.RFC3339, stamp)
			if err != nil || index >= len(result.Values) {
				continue
			}
			parsed.Points = append(parsed.Points, contract.MetricPoint{
				Timestamp: parsedAt.UTC(), Value: result.Values[index],
			})
		}
		series = append(series, parsed)
	}
	return series, nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// assert the interface.
var _ contract.MetricsConnector = (*CloudWatch)(nil)

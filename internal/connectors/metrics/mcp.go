package metrics

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/connectors/mcp"
)

// MCP calls the one read-only metrics tool an operator configures on a
// Streamable HTTP MCP server.
type MCP struct {
	client      *mcp.Client
	arguments   *base.Template
	cluster     string
	environment string
}

// NewMCP builds the MCP metrics Connector.
func NewMCP(client *mcp.Client, arguments *base.Template, clusterName, environment string) *MCP {
	return &MCP{client: client, arguments: arguments, cluster: clusterName, environment: environment}
}

// Name returns the connector name.
func (m *MCP) Name() string { return "mcp" }

// QueryMetrics calls the tool and parses either a {"series": [...]} envelope or
// "value timestamp" lines. Output is untrusted evidence.
func (m *MCP) QueryMetrics(ctx context.Context, q contract.MetricQuery) ([]contract.MetricSeries, error) {
	rendered, err := m.arguments.Render(contract.MetricFields(q, m.cluster, m.environment))
	if err != nil {
		return nil, err
	}
	text, err := m.client.Call(ctx, []byte(rendered))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Series []struct {
			Labels map[string]string `json:"labels"`
			Points [][2]any          `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err == nil && len(envelope.Series) > 0 {
		series := make([]contract.MetricSeries, 0, len(envelope.Series))
		for _, result := range envelope.Series {
			parsed := contract.MetricSeries{Labels: result.Labels}
			for _, point := range result.Points {
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
		return series, nil
	}
	parsed := contract.MetricSeries{Labels: map[string]string{"tool": m.client.Tool}}
	for line := range strings.SplitSeq(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		stamp, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		parsed.Points = append(parsed.Points, contract.MetricPoint{
			Timestamp: time.UnixMilli(stamp).UTC(), Value: value,
		})
	}
	if len(parsed.Points) == 0 {
		return nil, nil
	}
	return []contract.MetricSeries{parsed}, nil
}

// assert the interface.
var _ contract.MetricsConnector = (*MCP)(nil)

package logs

import (
	"context"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/connectors/mcp"
)

// MCP calls the one read-only log tool an operator configures on a Streamable
// HTTP MCP server. It is the escape hatch for a backend Pagerless does not
// implement, never the primary path.
type MCP struct {
	client      *mcp.Client
	arguments   *base.Template
	maxBytes    int64
	cluster     string
	environment string
}

// NewMCP builds the MCP log Connector.
func NewMCP(client *mcp.Client, arguments *base.Template, maxBytes int64, clusterName, environment string) *MCP {
	return &MCP{
		client: client, arguments: arguments, maxBytes: maxBytes, cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (m *MCP) Name() string { return "mcp" }

// QueryLogs calls the configured tool and treats each returned line as one log
// entry. Output is untrusted evidence: it is truncated and never interpreted.
func (m *MCP) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	rendered, err := m.arguments.Render(contract.LogFields(q, m.cluster, m.environment))
	if err != nil {
		return contract.LogPage{}, err
	}
	text, err := m.client.Call(ctx, []byte(rendered))
	if err != nil {
		return contract.LogPage{}, err
	}
	page := contract.LogPage{}
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			continue
		}
		page.Entries = append(page.Entries, contract.LogEntry{
			Timestamp: mcpTimestamp(line), Line: line, Stream: map[string]string{"tool": m.client.Tool},
		})
		page.Bytes += len(line)
	}
	page.Truncated = m.maxBytes > 0 && int64(page.Bytes) >= m.maxBytes
	return page, nil
}

// mcpTimestamp reads an RFC3339 prefix when the tool returns one.
func mcpTimestamp(line string) time.Time {
	first, _, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, first); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// assert the interface.
var _ contract.LogConnector = (*MCP)(nil)

package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// OpenSearch queries an OpenSearch cluster's _search API.
type OpenSearch struct {
	client      *base.Client
	index       *base.Template
	query       *base.Template
	limit       int
	apiKey      string
	cluster     string
	environment string
}

// NewOpenSearch builds the OpenSearch Connector.
func NewOpenSearch(
	client *base.Client, index, query *base.Template, limit int, apiKey string,
	clusterName, environment string,
) *OpenSearch {
	return &OpenSearch{
		client: client, index: index, query: query, limit: limit, apiKey: apiKey,
		cluster: clusterName, environment: environment,
	}
}

// Name returns the connector name.
func (o *OpenSearch) Name() string { return "opensearch" }

// QueryLogs searches the index. The configured query template, when present, is
// the whole query object; otherwise the Connector builds a bounded bool query
// that injects the namespace and the workload server-side.
func (o *OpenSearch) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	index, err := o.index.Render(contract.LogFields(q, o.cluster, o.environment))
	if err != nil {
		return contract.LogPage{}, err
	}
	if index == "" {
		return contract.LogPage{}, fmt.Errorf("the OpenSearch index template rendered empty")
	}
	query, err := o.buildQuery(q)
	if err != nil {
		return contract.LogPage{}, err
	}
	limit := o.limit
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	body, err := json.Marshal(map[string]any{
		"size":  limit,
		"query": query,
		"sort":  []any{map[string]any{timestampKey: map[string]string{"order": "desc"}}},
	})
	if err != nil {
		return contract.LogPage{}, err
	}
	headers := map[string]string{}
	if o.apiKey != "" {
		headers["Authorization"] = "ApiKey " + o.apiKey
	}
	response, err := o.client.Post(ctx, "/"+url.PathEscape(index)+"/_search", nil, body, "application/json", headers)
	if err != nil {
		return contract.LogPage{}, err
	}
	return decodeOpenSearchPage(response.Body, response.Truncated, limit)
}

// buildQuery renders the configured query or builds the default bool query.
func (o *OpenSearch) buildQuery(q contract.Query) (any, error) {
	if o.query != nil && o.query.Raw() != "" {
		rendered, err := o.query.Render(contract.LogFields(q, o.cluster, o.environment))
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
			return nil, fmt.Errorf("render the OpenSearch query: %w", err)
		}
		return decoded, nil
	}
	return map[string]any{
		"bool": map[string]any{
			"filter": []any{
				map[string]any{"term": map[string]string{"kubernetes.namespace": q.Namespace}},
				map[string]any{"wildcard": map[string]any{
					"kubernetes.pod_name": map[string]string{"value": q.Subject.Name + "-*"},
				}},
				map[string]any{"range": map[string]any{timestampKey: map[string]string{
					"gte": q.Since.UTC().Format(time.RFC3339), "lte": q.Until.UTC().Format(time.RFC3339),
				}}},
			},
		},
	}, nil
}

// openSearchResponse is the _search envelope that this Connector reads.
type openSearchResponse struct {
	Hits struct {
		Hits []struct {
			Source map[string]any `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// decodeOpenSearchPage converts hits into log entries.
func decodeOpenSearchPage(raw []byte, truncated bool, limit int) (contract.LogPage, error) {
	var decoded openSearchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return contract.LogPage{}, fmt.Errorf("decode the OpenSearch response: %w", err)
	}
	page := contract.LogPage{Truncated: truncated}
	for _, hit := range decoded.Hits.Hits {
		entry := contract.LogEntry{Stream: map[string]string{"index": "opensearch"}}
		entry.Timestamp = sourceTime(hit.Source)
		entry.Line = sourceMessage(hit.Source)
		entry.Severity = sourceString(hit.Source, "level", "severity")
		if kubernetes, ok := hit.Source["kubernetes"].(map[string]any); ok {
			for key, value := range kubernetes {
				if text, ok := value.(string); ok {
					entry.Stream[key] = text
				}
			}
		}
		page.Entries = append(page.Entries, entry)
		page.Bytes += len(entry.Line)
		if len(page.Entries) >= limit {
			page.Truncated = true
			break
		}
	}
	return page, nil
}

// sourceTime reads a timestamp from a document.
func sourceTime(source map[string]any) time.Time {
	for _, key := range []string{timestampKey, "timestamp", "time"} {
		value, ok := source[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, typed); err == nil {
				return parsed.UTC()
			}
		case float64:
			return time.UnixMilli(int64(typed)).UTC()
		}
	}
	return time.Time{}
}

// sourceMessage reads the log text from a document.
func sourceMessage(source map[string]any) string {
	for _, key := range []string{"message", "log", "msg"} {
		if value, ok := source[key].(string); ok {
			return value
		}
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// sourceString reads the first string value among keys.
func sourceString(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := source[key].(string); ok {
			return value
		}
	}
	return ""
}

// assert the interface.
var _ contract.LogConnector = (*OpenSearch)(nil)

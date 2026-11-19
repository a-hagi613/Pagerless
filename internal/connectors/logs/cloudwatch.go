package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/aws"
	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// logGroupNameKey is the log group parameter of a FilterLogEvents call.
const logGroupNameKey = "logGroupName"

// timestampKey is the CloudWatch timestamp field.
const timestampKey = "@timestamp"

// CloudWatch queries AWS CloudWatch Logs with SigV4 signing.
type CloudWatch struct {
	client       *base.Client
	caller       *aws.Caller
	region       string
	logGroup     *base.Template
	streamPrefix string
	filter       string
	mode         string
	limit        int
	cluster      string
	environment  string
	now          func() time.Time
}

// NewCloudWatch builds the CloudWatch Logs Connector.
func NewCloudWatch(
	client *base.Client, caller *aws.Caller, region string, logGroup *base.Template,
	streamPrefix, filter, mode string, limit int, clusterName, environment string,
) *CloudWatch {
	return &CloudWatch{
		client: client, caller: caller, region: region, logGroup: logGroup,
		streamPrefix: streamPrefix, filter: filter, mode: mode, limit: limit,
		cluster: clusterName, environment: environment, now: time.Now,
	}
}

// Name returns the connector name.
func (c *CloudWatch) Name() string { return "cloudwatch" }

// QueryLogs reads FilterLogEvents, or the documented StartQuery polling flow in
// insights mode.
func (c *CloudWatch) QueryLogs(ctx context.Context, q contract.Query) (contract.LogPage, error) {
	group, err := c.logGroup.Render(contract.LogFields(q, c.cluster, c.environment))
	if err != nil {
		return contract.LogPage{}, err
	}
	if group == "" {
		return contract.LogPage{}, fmt.Errorf("the CloudWatch log group template rendered empty")
	}
	limit := c.limit
	if q.Limit > 0 && q.Limit < limit {
		limit = q.Limit
	}
	if c.mode == "insights" {
		return c.insights(ctx, q, group, limit)
	}
	return c.filterEvents(ctx, q, group, limit)
}

// filterEvents reads one or more bounded FilterLogEvents pages.
func (c *CloudWatch) filterEvents(
	ctx context.Context, q contract.Query, group string, limit int,
) (contract.LogPage, error) {
	page := contract.LogPage{}
	token := ""
	for pages := 0; pages < c.client.MaxPages(); pages++ {
		request := map[string]any{
			logGroupNameKey: group,
			"startTime":     q.Since.UnixMilli(),
			"endTime":       q.Until.UnixMilli(),
			"limit":         limit,
		}
		if c.streamPrefix != "" {
			request["logStreamNamePrefix"] = c.streamPrefix
		}
		if c.filter != "" {
			request["filterPattern"] = c.filter
		}
		if token != "" {
			request["nextToken"] = token
		}
		body, err := json.Marshal(request)
		if err != nil {
			return page, err
		}
		raw, err := c.call(ctx, "Logs_20140328.FilterLogEvents", body)
		if err != nil {
			return page, err
		}
		var decoded struct {
			Events []struct {
				Timestamp     int64  `json:"timestamp"`
				Message       string `json:"message"`
				LogStreamName string `json:"logStreamName"`
			} `json:"events"`
			NextToken string `json:"nextToken"`
		}
		if err := json.Unmarshal(raw.Body, &decoded); err != nil {
			return page, fmt.Errorf("decode the CloudWatch response: %w", err)
		}
		for _, event := range decoded.Events {
			page.Entries = append(page.Entries, contract.LogEntry{
				Timestamp: time.UnixMilli(event.Timestamp).UTC(), Line: event.Message,
				Stream: map[string]string{"logStreamName": event.LogStreamName, logGroupNameKey: group},
			})
			page.Bytes += len(event.Message)
			if len(page.Entries) >= limit {
				page.Truncated = true
				return page, nil
			}
		}
		token = decoded.NextToken
		page.Truncated = page.Truncated || raw.Truncated
		if token == "" {
			break
		}
	}
	if token != "" {
		page.NextToken = token
		page.Truncated = true
	}
	return page, nil
}

// insights runs the two-call StartQuery/GetQueryResults flow. The second call
// is visible in the metrics and the audit trail by design.
func (c *CloudWatch) insights(
	ctx context.Context, q contract.Query, group string, limit int,
) (contract.LogPage, error) {
	start, err := json.Marshal(map[string]any{
		logGroupNameKey: group,
		"startTime":     q.Since.Unix(),
		"endTime":       q.Until.Unix(),
		"queryString":   "fields @timestamp, @message | sort @timestamp desc | limit " + strconv.Itoa(limit),
	})
	if err != nil {
		return contract.LogPage{}, err
	}
	raw, err := c.call(ctx, "Logs_20140328.StartQuery", start)
	if err != nil {
		return contract.LogPage{}, err
	}
	var started struct {
		QueryID string `json:"queryId"`
	}
	if err := json.Unmarshal(raw.Body, &started); err != nil || started.QueryID == "" {
		return contract.LogPage{}, fmt.Errorf("CloudWatch StartQuery returned no query id")
	}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return contract.LogPage{}, fmt.Errorf("CloudWatch query did not complete: %w", ctx.Err())
		case <-deadline.C:
			return contract.LogPage{}, fmt.Errorf("CloudWatch query did not complete within 20s")
		case <-ticker.C:
		}
		body, err := json.Marshal(map[string]string{"queryId": started.QueryID})
		if err != nil {
			return contract.LogPage{}, err
		}
		results, err := c.call(ctx, "Logs_20140328.GetQueryResults", body)
		if err != nil {
			return contract.LogPage{}, err
		}
		page, done, err := decodeInsights(results.Body, limit)
		if err != nil {
			return contract.LogPage{}, err
		}
		if done {
			page.Truncated = page.Truncated || results.Truncated
			return page, nil
		}
	}
}

// decodeInsights converts a GetQueryResults response. done is false while the
// query is still scheduled or running.
func decodeInsights(raw []byte, limit int) (contract.LogPage, bool, error) {
	var decoded struct {
		Status  string            `json:"status"`
		Results [][]insightResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return contract.LogPage{}, false, fmt.Errorf("decode the CloudWatch Insights response: %w", err)
	}
	switch decoded.Status {
	case "Scheduled", "Running", "Unknown":
		return contract.LogPage{}, false, nil
	case "Complete":
	default:
		return contract.LogPage{}, false, fmt.Errorf("CloudWatch query ended with status %s", decoded.Status)
	}
	page := contract.LogPage{}
	for _, row := range decoded.Results {
		entry := contract.LogEntry{Stream: map[string]string{"source": "cloudwatch-insights"}}
		for _, field := range row {
			switch field.Field {
			case timestampKey:
				if parsed, err := time.Parse(time.RFC3339Nano, field.Value); err == nil {
					entry.Timestamp = parsed.UTC()
				}
			case "@message":
				entry.Line = field.Value
			default:
				entry.Stream[field.Field] = field.Value
			}
		}
		page.Entries = append(page.Entries, entry)
		page.Bytes += len(entry.Line)
		if len(page.Entries) >= limit {
			page.Truncated = true
			break
		}
	}
	return page, true, nil
}

// insightResult is one field of one Insights row.
type insightResult struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// call signs and performs one CloudWatch API call.
func (c *CloudWatch) call(ctx context.Context, target string, body []byte) (base.Response, error) {
	return c.caller.Call(ctx, target, body)
}

// assert the interface.
var _ contract.LogConnector = (*CloudWatch)(nil)

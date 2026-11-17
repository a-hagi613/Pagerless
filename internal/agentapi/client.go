package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is a Go implementation of the agent side of the contract. The
// production agent is written in Python; this client lets Go tests and
// tooling speak the same protocol.
type Client struct {
	// BaseURL is the base URL of pagerless-server's agent API.
	BaseURL string
	// Triage is the name of the Triage the agent belongs to.
	Triage string
	// Token returns the current projected service account token.
	Token func() (string, error)
	// HTTPClient performs the requests. It defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Result is the server's answer to a request.
type Result struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Report is the decoded answer to a report, when the server sent one.
	Report *ReportResponse
	// Message is the decoded error message, when the server sent one.
	Message string
}

// PostEvent sends one step event.
func (c *Client) PostEvent(ctx context.Context, e StepEvent) (Result, error) {
	return c.post(ctx, EventsPath(c.Triage), e)
}

// PostReport sends the final report.
func (c *Client) PostReport(ctx context.Context, r Report) (Result, error) {
	return c.post(ctx, ReportPath(c.Triage), r)
}

func (c *Client) post(ctx context.Context, path string, body any) (Result, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("encode request: %w", err)
	}
	return c.do(ctx, path, payload)
}

// PostRaw sends an arbitrary body to a path, for callers that need to send
// malformed input.
func (c *Client) PostRaw(ctx context.Context, path string, payload []byte) (Result, error) {
	return c.do(ctx, path, payload)
}

func (c *Client) do(ctx context.Context, path string, payload []byte) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return Result{}, fmt.Errorf("read token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}
	result := Result{StatusCode: resp.StatusCode}
	var report ReportResponse
	if json.Unmarshal(raw, &report) == nil && (report.Accepted || len(report.Errors) > 0 || report.Phase != "") {
		result.Report = &report
	}
	var failure ErrorResponse
	if json.Unmarshal(raw, &failure) == nil {
		result.Message = failure.Message
	}
	return result, nil
}

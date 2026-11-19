// Package mcp is a minimal Streamable HTTP client for the one read-only MCP
// tool an operator configures. stdio is not offered: a single in-cluster binary
// cannot spawn arbitrary processes.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// ProtocolVersion is the pinned MCP protocol version.
const ProtocolVersion = "2025-06-18"

// jsonRPCVersion is the only JSON-RPC version the bridge speaks.
const jsonRPCVersion = "2.0"

// jsonRPCField is the JSON-RPC version field.
const jsonRPCField = "jsonrpc"

// methodField is the JSON-RPC method field.
const methodField = "method"

// Doer performs one HTTP request through the Connector guard.
type Doer interface {
	Do(
		ctx context.Context, method, absoluteURL string, body []byte, contentType string,
		headers map[string]string,
	) (contract.Response, error)
}

// Client calls one configured tool on one MCP server.
type Client struct {
	// Doer performs the HTTP requests.
	Doer Doer
	// URL is the Streamable HTTP endpoint.
	URL string
	// Tool is the single allowlisted tool name.
	Tool string
	// Version is the client version reported in initialize.
	Version string
}

// Call initializes a session, then invokes the tool. Only the configured tool
// can be invoked: the tool name is never taken from model output.
func (c *Client) Call(ctx context.Context, arguments []byte) (string, error) {
	headers := map[string]string{
		"Accept":               "application/json, text/event-stream",
		"MCP-Protocol-Version": ProtocolVersion,
	}
	if len(arguments) == 0 {
		arguments = []byte("{}")
	}
	var parsed map[string]any
	if err := json.Unmarshal(arguments, &parsed); err != nil {
		return "", fmt.Errorf("the MCP arguments are not a JSON object: %w", err)
	}
	initialize, err := json.Marshal(map[string]any{
		jsonRPCField: jsonRPCVersion, "id": 1, methodField: "initialize",
		"params": map[string]any{
			"protocolVersion": ProtocolVersion, "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "pagerless", "version": c.Version},
		},
	})
	if err != nil {
		return "", err
	}
	response, err := c.Doer.Do(ctx, "POST", c.URL, initialize, "application/json", headers)
	if err != nil {
		return "", err
	}
	session := response.Header.Get("Mcp-Session-Id")
	if session != "" {
		headers["Mcp-Session-Id"] = session
	}
	initialized, err := json.Marshal(map[string]any{
		jsonRPCField: jsonRPCVersion, methodField: "notifications/initialized",
	})
	if err != nil {
		return "", err
	}
	if _, err := c.Doer.Do(ctx, "POST", c.URL, initialized, "application/json", headers); err != nil {
		return "", err
	}
	call, err := json.Marshal(map[string]any{
		jsonRPCField: jsonRPCVersion, "id": 2, methodField: "tools/call",
		"params": map[string]any{"name": c.Tool, "arguments": parsed},
	})
	if err != nil {
		return "", err
	}
	result, err := c.Doer.Do(ctx, "POST", c.URL, call, "application/json", headers)
	if err != nil {
		return "", err
	}
	return decodeResult(result.Body)
}

// decodeResult reads a JSON or SSE JSON-RPC response and returns the text
// content of a successful tool call.
func decodeResult(raw []byte) (string, error) {
	payload := raw
	if bytes.Contains(raw, []byte("data:")) {
		var builder strings.Builder
		for line := range strings.SplitSeq(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			data, found := strings.CutPrefix(trimmed, "data:")
			if !found {
				continue
			}
			builder.WriteString(strings.TrimSpace(data))
		}
		payload = []byte(builder.String())
	}
	var decoded struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode the MCP response: %w", err)
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("MCP tool call failed: %s", decoded.Error.Message)
	}
	if decoded.Result.IsError {
		return "", fmt.Errorf("MCP tool reported an error")
	}
	texts := make([]string, 0, len(decoded.Result.Content))
	for _, content := range decoded.Result.Content {
		if content.Type == "text" && content.Text != "" {
			texts = append(texts, content.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

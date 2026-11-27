// Package mcp is a bounded Streamable HTTP client and the capability policies
// the investigation service applies to the integrations it exposes.
//
// The client speaks JSON-RPC 2.0 over HTTP, accepts both a JSON response and an
// SSE response, and refuses a body larger than its configured bound. The
// policies are the second line of defense: a model sees only the tool
// descriptions the policy admits, and every call is validated again before it
// reaches a server, because tool discovery never grants a capability by itself.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion is the MCP revision this client pins.
const ProtocolVersion = "2025-06-18"

const (
	jsonRPCVersion = "2.0"
	// keyToolName is the JSON field used by client information and tool names.
	keyToolName = "name"
	// DefaultMaxResponseBytes bounds one JSON-RPC response.
	DefaultMaxResponseBytes = 4 << 20
	// defaultTimeout bounds one request when the client has no deadline.
	defaultTimeout = 30 * time.Second
)

// Tool is one advertised MCP tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result is one tool outcome. Text is the concatenation of the text content
// items; IsError reports that the server marked the call as failed.
type Result struct {
	Text    string
	IsError bool
}

// RPCError is a JSON-RPC or transport failure.
type RPCError struct {
	Code    int
	Message string
}

// Error renders the RPC error.
func (e *RPCError) Error() string { return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code) }

// Client is one MCP endpoint. It is safe for concurrent use; session setup is
// serialized and performed at most once.
type Client struct {
	// Name identifies the integration in logs and errors.
	Name string
	// Endpoint is the Streamable HTTP URL.
	Endpoint string
	// Token returns the bearer credential for each request. It is called per
	// request so a rotated projected token is picked up without a restart.
	Token func(context.Context) (string, error)
	// HTTPClient performs requests. It must have a timeout or receive a deadline.
	HTTPClient *http.Client
	// MaxResponseBytes bounds one response body.
	MaxResponseBytes int64

	mu      sync.Mutex
	session string
	ready   bool
	nextID  int64
}

// New returns a client with the protocol defaults.
func New(name, endpoint string, token func(context.Context) (string, error), httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{Name: name, Endpoint: endpoint, Token: token, HTTPClient: httpClient}
}

func (c *Client) maxBytes() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseBytes
}

// Initialize performs the MCP handshake once. A later call is a no-op.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initializeLocked(ctx)
}

func (c *Client) initializeLocked(ctx context.Context) error {
	if c.ready {
		return nil
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("mcp: the %s endpoint is not configured", c.Name)
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{keyToolName: "pagerless", "version": "1.0"},
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.callLocked(ctx, "initialize", params, &result); err != nil {
		return err
	}
	if result.ProtocolVersion != "" && result.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("mcp: the %s server speaks protocol %q, this client pins %q",
			c.Name, result.ProtocolVersion, ProtocolVersion)
	}
	if err := c.notifyLocked(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	c.ready = true
	return nil
}

// ListTools returns the tools the server advertises.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initializeLocked(ctx); err != nil {
		return nil, err
	}
	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.callLocked(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	if result.Tools == nil {
		return nil, fmt.Errorf("mcp: the %s server returned no tool list", c.Name)
	}
	return result.Tools, nil
}

// CallTool invokes one tool and returns its text content.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initializeLocked(ctx); err != nil {
		return Result{}, err
	}
	var parsed any = map[string]any{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return Result{}, fmt.Errorf("mcp: the %s arguments are not JSON: %w", name, err)
		}
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	params := map[string]any{keyToolName: name, "arguments": parsed}
	if err := c.callLocked(ctx, "tools/call", params, &result); err != nil {
		return Result{}, err
	}
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if content.Type == "text" {
			texts = append(texts, content.Text)
		}
	}
	return Result{Text: strings.Join(texts, "\n"), IsError: result.IsError}, nil
}

// callLocked sends one request and decodes its result. The caller holds c.mu.
func (c *Client) callLocked(ctx context.Context, method string, params, result any) error {
	id := c.nextID + 1
	c.nextID = id
	request := map[string]any{jsonRPCField: jsonRPCVersion, "id": id, methodField: method, "params": params}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("mcp: encode the %s request: %w", method, err)
	}
	response, err := c.post(ctx, payload)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	document, err := readResponse(response, c.maxBytes())
	if err != nil {
		return err
	}
	envelope, err := decodeEnvelope(document, id)
	if err != nil {
		return err
	}
	if envelope.Error != nil {
		return &RPCError{Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("mcp: the %s server returned no result for %s", c.Name, method)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("mcp: decode the %s result: %w", method, err)
	}
	return nil
}

// notifyLocked sends a JSON-RPC notification. A server may answer 202 with no
// body, which is success.
func (c *Client) notifyLocked(ctx context.Context, method string, params any) error {
	payload, err := json.Marshal(map[string]any{jsonRPCField: jsonRPCVersion, methodField: method, "params": params})
	if err != nil {
		return fmt.Errorf("mcp: encode the %s notification: %w", method, err)
	}
	response, err := c.post(ctx, payload)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("mcp: the %s server refused %s with HTTP %d", c.Name, method, response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

const (
	jsonRPCField = "jsonrpc"
	methodField  = "method"
)

func (c *Client) post(ctx context.Context, payload []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mcp: build the %s request: %w", c.Name, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if c.Token != nil {
		token, err := c.Token(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp: read the %s credential: %w", c.Name, err)
		}
		if token == "" {
			return nil, fmt.Errorf("mcp: the %s credential is empty", c.Name)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if c.session != "" {
		request.Header.Set("Mcp-Session-Id", c.session)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("mcp: call the %s server: %w", c.Name, err)
	}
	if session := response.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		_ = response.Body.Close()
		return nil, fmt.Errorf("mcp: the %s server answered HTTP %d: %s",
			c.Name, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return response, nil
}

// envelope is one JSON-RPC response.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// readResponse reads a JSON or SSE response body within limit bytes.
func readResponse(response *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("mcp: read the response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("mcp: the response exceeds %d bytes", limit)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		document, err := lastEvent(body)
		if err != nil {
			return nil, err
		}
		return document, nil
	}
	return body, nil
}

// lastEvent returns the last complete SSE data payload. A Streamable HTTP
// server sends one JSON-RPC response per POST, so the last event is the answer.
func lastEvent(body []byte) ([]byte, error) {
	var (
		current bytes.Buffer
		last    []byte
	)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), len(body)+1)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if current.Len() > 0 {
				last = append(last[:0], current.Bytes()...)
				current.Reset()
			}
			continue
		}
		data, found := strings.CutPrefix(line, "data:")
		if !found {
			continue
		}
		current.WriteString(strings.TrimSpace(data))
	}
	if current.Len() > 0 {
		last = append(last[:0], current.Bytes()...)
	}
	if len(last) == 0 {
		return nil, errors.New("mcp: the SSE response carried no data")
	}
	return last, nil
}

// decodeEnvelope decodes the JSON-RPC response for id. It skips batch entries
// and unrelated ids, which a proxy may interleave.
func decodeEnvelope(document []byte, id int64) (envelope, error) {
	trimmed := bytes.TrimSpace(document)
	if len(trimmed) == 0 {
		return envelope{}, errors.New("mcp: the server returned an empty response")
	}
	if trimmed[0] == '[' {
		var batch []envelope
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return envelope{}, fmt.Errorf("mcp: decode the response batch: %w", err)
		}
		for _, item := range batch {
			if matchesID(item.ID, id) {
				return item, nil
			}
		}
		return envelope{}, fmt.Errorf("mcp: the response batch does not answer request %d", id)
	}
	var decoded envelope
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return envelope{}, fmt.Errorf("mcp: decode the response: %w", err)
	}
	if !matchesID(decoded.ID, id) {
		return envelope{}, fmt.Errorf("mcp: the response does not answer request %d", id)
	}
	return decoded, nil
}

func matchesID(raw json.RawMessage, id int64) bool {
	var value int64
	if err := json.Unmarshal(raw, &value); err == nil {
		return value == id
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text == fmt.Sprint(id)
	}
	return false
}

// Package agentclient is the private Go client of the Python investigation
// agent. It implements the installer-authenticated /turn, /turns/{turnId}/stop
// and /readyz contract.
package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBody bounds one agent response.
const maxBody = 8 << 20

// Message is one turn message in the private contract.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	TurnID  string `json:"turnId,omitempty"`
}

// Target is the optional Kubernetes hint.
type Target struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Tool is one admitted tool description.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Limits lowers the agent's configured budgets for one turn.
type Limits struct {
	ToolCalls   int `json:"toolCalls,omitempty"`
	ModelCalls  int `json:"modelCalls,omitempty"`
	InputTokens int `json:"inputTokens,omitempty"`
	Seconds     int `json:"seconds,omitempty"`
}

// Request is the POST /turn body.
type Request struct {
	PageID      string    `json:"pageId"`
	TurnID      string    `json:"turnId"`
	Messages    []Message `json:"messages"`
	Namespace   string    `json:"namespace,omitempty"`
	Target      *Target   `json:"target,omitempty"`
	Tools       []Tool    `json:"tools"`
	CallbackURL string    `json:"callbackUrl"`
	Limits      *Limits   `json:"limits,omitempty"`
}

// Citation is one evidence reference in a terminal answer.
type Citation struct {
	CallID  string `json:"callId"`
	Pointer string `json:"pointer,omitempty"`
}

// Usage is the model and tool usage of one turn.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	ModelCalls   int `json:"modelCalls"`
	ToolCalls    int `json:"toolCalls"`
}

// ProposalFile is one proposed file content.
type ProposalFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Proposal is the optional source change of a completed turn.
type Proposal struct {
	BaseSHA string         `json:"baseSha"`
	Files   []ProposalFile `json:"files"`
}

// Failure is the error object of a failed turn.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Terminal is the terminal JSON object of POST /turn.
type Terminal struct {
	Status    string     `json:"status"`
	Content   string     `json:"content"`
	Citations []Citation `json:"citations"`
	Usage     Usage      `json:"usage"`
	Proposal  *Proposal  `json:"proposal"`
	Error     *Failure   `json:"error"`
}

// Check is one readiness check reported by the agent.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// Readiness is the agent's GET /readyz body.
type Readiness struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

// StatusError is a definitive or ambiguous failure of the private interface.
type StatusError struct {
	// HTTPStatus is the response status, or zero for a transport failure.
	HTTPStatus int
	// Code is the error code the agent returned, when it returned one.
	Code string
	// Message is the agent's message or the transport error.
	Message string
	// Ambiguous is true when the request may have been accepted and executed.
	Ambiguous bool
}

// Error renders the failure.
func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("agent: %s (HTTP %d): %s", e.Code, e.HTTPStatus, e.Message)
	}
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("agent: HTTP %d: %s", e.HTTPStatus, e.Message)
	}
	return "agent: " + e.Message
}

// Client calls one agent base URL.
type Client struct {
	// BaseURL is the in-cluster service root, for example
	// http://pagerless-agent:8090.
	BaseURL string
	// Token returns the installer-provided internal bearer credential.
	Token func(context.Context) (string, error)
	// HTTP performs requests. Its timeout should exceed one turn, so the turn
	// deadline comes from the request context instead.
	HTTP *http.Client
}

// New builds a client.
func New(baseURL string, token func(context.Context) (string, error), client *http.Client) *Client {
	if client == nil {
		client = &http.Client{}
	}
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token, HTTP: client}
}

// StartTurn sends one turn and waits for its terminal answer. The context
// bounds the whole turn; cancelling it aborts the HTTP request.
func (c *Client) StartTurn(ctx context.Context, request Request) (Terminal, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return Terminal{}, &StatusError{Message: "the agent URL is not configured", Ambiguous: false}
	}
	var terminal Terminal
	status, err := c.do(ctx, http.MethodPost, "/turn", request, &terminal)
	if err != nil {
		return Terminal{}, err
	}
	if status != http.StatusOK {
		return Terminal{}, &StatusError{
			HTTPStatus: status,
			Code:       terminal.Error.code(),
			Message:    terminal.Error.message(),
			Ambiguous:  status >= 500 || status == http.StatusConflict,
		}
	}
	if err := terminal.validate(); err != nil {
		return Terminal{}, err
	}
	return terminal, nil
}

// StopTurn asks the agent to stop a turn. The status is stopped, finished or
// unknown, as the private contract defines.
func (c *Client) StopTurn(ctx context.Context, turnID string) (string, error) {
	var response struct {
		Status string   `json:"status"`
		Error  *Failure `json:"error"`
	}
	status, err := c.do(ctx, http.MethodPost, "/turns/"+turnID+"/stop", map[string]any{}, &response)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", &StatusError{HTTPStatus: status, Code: response.Error.code(),
			Message: response.Error.message(), Ambiguous: true}
	}
	switch response.Status {
	case "stopped", "finished", "unknown":
		return response.Status, nil
	default:
		return "", &StatusError{HTTPStatus: status, Message: "the agent returned an unknown stop status"}
	}
}

// Ready fetches the agent readiness body. A 503 still carries the body, so the
// degraded checks can be reported.
func (c *Client) Ready(ctx context.Context) (Readiness, error) {
	var readiness Readiness
	status, err := c.do(ctx, http.MethodGet, "/readyz", nil, &readiness)
	if err != nil {
		return Readiness{}, err
	}
	if status != http.StatusOK && status != http.StatusServiceUnavailable {
		return Readiness{}, &StatusError{HTTPStatus: status, Message: "the readiness route is unavailable"}
	}
	if readiness.Status != "ready" && readiness.Status != "degraded" {
		return Readiness{}, &StatusError{HTTPStatus: status, Message: "the readiness body has no valid status"}
	}
	return readiness, nil
}

// do performs one request. It returns the status and decodes the body into
// result when result is non-nil. Transport failures are wrapped as ambiguous,
// because the request may have reached the agent.
func (c *Client) do(ctx context.Context, method, path string, payload, result any) (int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("agent: encode the request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("agent: build the request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		token, err := c.Token(ctx)
		if err != nil {
			return 0, fmt.Errorf("agent: read the internal credential: %w", err)
		}
		if token == "" {
			return 0, errors.New("agent: the internal credential is empty")
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return 0, &StatusError{Message: err.Error(), Ambiguous: true}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return response.StatusCode, &StatusError{HTTPStatus: response.StatusCode, Message: err.Error(), Ambiguous: true}
	}
	if len(raw) > maxBody {
		return response.StatusCode, &StatusError{
			HTTPStatus: response.StatusCode, Message: "the response exceeds the size bound", Ambiguous: true,
		}
	}
	if result != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			// A non-success body may be a plain error object; the caller decides
			// whether the status is meaningful.
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return response.StatusCode, &StatusError{
					HTTPStatus: response.StatusCode, Message: "the response is not JSON", Ambiguous: true,
				}
			}
		}
	}
	if response.StatusCode >= 500 {
		return response.StatusCode, &StatusError{
			HTTPStatus: response.StatusCode, Message: snippet(raw), Ambiguous: true,
		}
	}
	return response.StatusCode, nil
}

func (f *Failure) code() string {
	if f == nil {
		return ""
	}
	return f.Code
}

func (f *Failure) message() string {
	if f == nil {
		return ""
	}
	return f.Message
}

// validate rejects a terminal object that does not match the private contract.
func (t Terminal) validate() error {
	if t.Status != "completed" && t.Status != "stopped" && t.Status != "failed" {
		return &StatusError{HTTPStatus: http.StatusOK, Message: "the terminal status is not valid", Ambiguous: true}
	}
	if t.Status == "failed" && (t.Error == nil || t.Error.Message == "") {
		return &StatusError{HTTPStatus: http.StatusOK, Message: "a failed turn carries no error", Ambiguous: true}
	}
	for _, citation := range t.Citations {
		if strings.TrimSpace(citation.CallID) == "" {
			return &StatusError{HTTPStatus: http.StatusOK, Message: "a citation has no callId", Ambiguous: true}
		}
	}
	if t.Proposal != nil {
		if strings.TrimSpace(t.Proposal.BaseSHA) == "" || len(t.Proposal.Files) == 0 {
			return &StatusError{HTTPStatus: http.StatusOK, Message: "the proposal is incomplete", Ambiguous: true}
		}
	}
	return nil
}

func snippet(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

// Timeout bounds one stop request.
const StopTimeout = 5 * time.Second

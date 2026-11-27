package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxResponseBytes bounds one JSON answer so a broken server cannot exhaust
	// memory.
	maxResponseBytes = 8 << 20
	// maxEventBytes bounds one streamed event, including its data lines.
	maxEventBytes = 1 << 20
	// maxErrorBytes bounds the body read to explain an error status.
	maxErrorBytes = 1 << 20
)

// client calls the investigation API. It holds the operator token for the
// lifetime of one command and never puts it in a URL, a message, or a log.
type client struct {
	base       *url.URL
	http       *http.Client
	streamHTTP *http.Client
	token      string
	timeout    time.Duration
	userAgent  string
}

// newClient validates an explicit base address and token. The address may
// carry a path prefix for a reverse proxy, but not credentials.
func newClient(address, token string, timeout time.Duration) (*client, error) {
	parsed, err := parseAddress(address)
	if err != nil {
		return nil, err
	}
	transport := newTransport(timeout)
	return &client{
		base:       parsed,
		http:       &http.Client{Transport: transport, Timeout: timeout},
		streamHTTP: &http.Client{Transport: transport},
		token:      token,
		timeout:    timeout,
		userAgent:  "kubectl-page/" + Version,
	}, nil
}

// parseAddress validates an operator-supplied base URL.
func parseAddress(address string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, usagef("the server address must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, usagef("the server address must use http or https")
	}
	if parsed.User != nil {
		return nil, usagef("the server address must not carry credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, usagef("the server address must not carry a query or fragment")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed, nil
}

// newTransport bounds connection setup and header waits. A streamed response is
// bounded by its command context instead of a whole-response timeout.
func newTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: timeout,
	}
}

// request builds one API request. The escaped path is assembled as a URL so a
// page ID or call ID containing a slash cannot address another resource.
func (c *client) request(
	ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string,
) (*http.Request, error) {
	endpoint, err := url.Parse(c.base.String() + path)
	if err != nil {
		return nil, fmt.Errorf("build the request URL: %w", err)
	}
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", c.userAgent)
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return request, nil
}

// do performs one bounded JSON request and decodes the answer into out.
func (c *client) do(
	ctx context.Context, method, path string, query url.Values, body, out any, idempotencyKey string,
) error {
	request, err := c.request(ctx, method, path, query, body, idempotencyKey)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return c.transportError(method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := readBounded(response.Body, maxResponseBytes)
	if err != nil {
		return fmt.Errorf("read the %s %s response: %w", method, path, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return newAPIError(response.StatusCode, raw)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode the %s %s response: %w", method, path, err)
	}
	return nil
}

// openEvents starts a server-sent event stream positioned after the given
// sequence. The caller owns the body and must close it.
func (c *client) openEvents(ctx context.Context, pageID string, after int64) (io.ReadCloser, error) {
	query := url.Values{"after": {strconv.FormatInt(after, 10)}}
	request, err := c.request(ctx, http.MethodGet, pagePath(pageID, "events"), query, nil, "")
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	if after > 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	response, err := c.streamHTTP.Do(request)
	if err != nil {
		return nil, c.transportError(http.MethodGet, pagePath(pageID, "events"), err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		raw, _ := readBounded(response.Body, maxErrorBytes)
		_ = response.Body.Close()
		return nil, newAPIError(response.StatusCode, raw)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		raw, _ := readBounded(response.Body, maxErrorBytes)
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: got %q: %s",
			errStreamProtocol, contentType, truncate(sanitize(string(raw)), 200))
	}
	return response.Body, nil
}

// transportError turns a failed round trip into an explanation a person can act
// on, while keeping the original error for errors.Is.
func (c *client) transportError(method, path string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("%s %s timed out after %s: %w", method, path, c.timeout, err)
	}
	return fmt.Errorf("%s %s: %w", method, path, err)
}

// readBounded reads at most limit bytes and fails rather than truncating.
func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("the response exceeds the %d byte limit", limit)
	}
	return raw, nil
}

// apiError is one documented error object with its HTTP status.
type apiError struct {
	Status  int
	Code    string
	Message string
}

// newAPIError decodes the error envelope, falling back to the status text when
// the body is not the documented shape.
func newAPIError(status int, raw []byte) *apiError {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	code, message := "", ""
	if err := json.Unmarshal(raw, &envelope); err == nil {
		code, message = envelope.Error.Code, envelope.Error.Message
	}
	message = truncate(sanitize(message), 2000)
	if message == "" {
		message = http.StatusText(status)
	}
	api := &apiError{Status: status, Code: code, Message: message}
	api.addGuidance()
	return api
}

// addGuidance appends the action that resolves the common failures. It never
// names a token or a URL that could contain one.
func (e *apiError) addGuidance() {
	switch e.Status {
	case http.StatusUnauthorized:
		e.Message += " (set PAGERLESS_TOKEN or pass --token-file with the operator token)"
	case http.StatusForbidden:
		e.Message += " (this operator token is not allowed to read the installation)"
	case http.StatusConflict:
		e.Message += " (another turn is active for this page; follow or stop it)"
	case http.StatusTooManyRequests:
		e.Message += " (the installation is at capacity; retry later)"
	}
}

// Error renders the code and message without the HTTP status noise.
func (e *apiError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// exitCode maps a documented API failure to the CLI's contract.
func (e *apiError) exitCode() int {
	switch e.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ExitUsage
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusConflict:
		return ExitConflict
	default:
		return ExitFailure
	}
}

// retryable reports whether reconnecting could deliver the same request again.
// Client errors and authentication failures will not improve.
func (e *apiError) retryable() bool {
	return e.Status >= http.StatusInternalServerError || e.Status == http.StatusTooManyRequests
}

// pagePath escapes every identifier so an opaque ID cannot change the route.
func pagePath(id string, suffix ...string) string {
	var path strings.Builder
	path.WriteString(apiPrefix + "/pages/")
	path.WriteString(url.PathEscape(id))
	for _, part := range suffix {
		path.WriteString("/")
		path.WriteString(url.PathEscape(part))
	}
	return path.String()
}

// newIdempotencyKey returns a fresh key for one create or reply attempt.
func newIdempotencyKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate an idempotency key: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// createPage opens a new conversation.
func (c *client) createPage(ctx context.Context, request createRequest) (createResponse, error) {
	var response createResponse
	key, err := newIdempotencyKey()
	if err != nil {
		return response, err
	}
	err = c.do(ctx, http.MethodPost, apiPrefix+"/pages", nil, request, &response, key)
	return response, err
}

// createTurn adds a turn to an existing conversation.
func (c *client) createTurn(ctx context.Context, pageID string, request createRequest) (createResponse, error) {
	var response createResponse
	key, err := newIdempotencyKey()
	if err != nil {
		return response, err
	}
	err = c.do(ctx, http.MethodPost, pagePath(pageID, "turns"), nil, request, &response, key)
	return response, err
}

// page reads one conversation with its messages.
func (c *client) page(ctx context.Context, pageID string) (pageDetail, error) {
	var detail pageDetail
	if err := c.do(ctx, http.MethodGet, pagePath(pageID), nil, nil, &detail, ""); err != nil {
		return detail, err
	}
	if detail.Messages == nil {
		detail.Messages = []message{}
	}
	for i := range detail.Messages {
		if detail.Messages[i].Citations == nil {
			detail.Messages[i].Citations = []citation{}
		}
	}
	return detail, nil
}

// pages lists the installation's conversations, newest activity first.
func (c *client) pages(ctx context.Context) ([]pageSummary, error) {
	var list struct {
		Pages []pageSummary `json:"pages"`
	}
	if err := c.do(ctx, http.MethodGet, apiPrefix+"/pages", nil, nil, &list, ""); err != nil {
		return nil, err
	}
	return list.Pages, nil
}

// stopTurn asks the server to stop the active turn. Repeating it is safe.
func (c *client) stopTurn(ctx context.Context, pageID string) (stopResponse, error) {
	var response stopResponse
	err := c.do(ctx, http.MethodPost, pagePath(pageID, "stop"), nil, nil, &response, "")
	return response, err
}

// evidence reads one sanitized tool attempt.
func (c *client) evidence(ctx context.Context, pageID, callID string) (evidence, error) {
	var record evidence
	err := c.do(ctx, http.MethodGet, pagePath(pageID, "evidence", callID), nil, nil, &record, "")
	return record, err
}

// diff reads the proposal for a page.
func (c *client) diff(ctx context.Context, pageID string) (diffResult, error) {
	var result diffResult
	err := c.do(ctx, http.MethodGet, pagePath(pageID, "diff"), nil, nil, &result, "")
	return result, err
}

// events reads the ordered log once, without following.
func (c *client) events(ctx context.Context, pageID string, after int64) ([]event, error) {
	var list eventList
	query := url.Values{"after": {strconv.FormatInt(after, 10)}}
	if err := c.do(ctx, http.MethodGet, pagePath(pageID, "events"), query, nil, &list, ""); err != nil {
		return nil, err
	}
	return list.Events, nil
}

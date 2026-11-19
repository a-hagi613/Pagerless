package base

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Sentinel errors that a caller maps to a degradation note rather than a Page
// failure.
var (
	// ErrBreakerOpen means the Connector's Circuit Breaker is open.
	ErrBreakerOpen = errors.New("connector circuit breaker is open")
	// ErrRateLimited means the Connector's token bucket is empty.
	ErrRateLimited = errors.New("connector rate limit exceeded")
	// ErrRedirectRefused means the backend tried to redirect the Connector.
	ErrRedirectRefused = errors.New("connector refused a redirect")
)

// StatusError is a non-2xx backend response.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the bounded response body, for the operator's log.
	Body string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("backend returned HTTP %d: %s", e.Status, truncateText(e.Body, 300))
}

// blockedPrefixes are the address ranges a Connector may never reach: the
// link-local ranges that serve cloud instance metadata, and the documented
// metadata addresses inside provider-specific ranges.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("100.100.100.200/32"),
}

// blockedHosts are hostile names that resolve only to metadata services.
var blockedHosts = []string{"metadata", "metadata.google.internal", "instance-data"}

// Response is one bounded backend response.
type Response = contract.Response

// ClientOptions configures one read-only HTTP client.
type ClientOptions struct {
	// Name is the Connector name.
	Name string
	// BaseURL is the backend base URL. It may be empty for a client that is
	// only used with absolute URLs, such as the CloudWatch endpoint.
	BaseURL string
	// Auth is the credential reference.
	Auth Auth
	// Policy is the timeout, byte cap, retry and overload policy.
	Policy Policy
	// Credentials resolves the reference.
	Credentials Credentials
	// AllowedHosts, when non-empty, is the only host set the client may reach.
	AllowedHosts []string
	// Metrics records health.
	Metrics *Metrics
	// Now defaults to time.Now, for tests.
	Now func() time.Time
	// Transport overrides the default transport, for tests that serve a plain
	// HTTP server with a custom dialer.
	Transport http.RoundTripper
}

// Client is a guarded, read-only HTTP client for one Connector.
type Client struct {
	name        string
	base        *url.URL
	auth        Auth
	policy      Policy
	credentials Credentials
	allowed     []string
	metrics     *Metrics
	http        *http.Client
	limiter     *Limiter
	breaker     *Breaker
}

// NewClient builds a Connector client. It refuses a non-HTTP base URL, a URL
// with embedded credentials, and a timeout or cap outside the documented
// bounds.
func NewClient(options ClientOptions) (*Client, error) {
	if options.Name == "" {
		return nil, errors.New("a Connector client needs a name")
	}
	var base *url.URL
	if options.BaseURL != "" {
		parsed, err := url.Parse(options.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("connector %s: %w", options.Name, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("connector %s: the base URL must use http or https", options.Name)
		}
		if parsed.User != nil {
			return nil, fmt.Errorf("connector %s: the base URL must not embed credentials", options.Name)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("connector %s: the base URL needs a host", options.Name)
		}
		if err := checkHostAllowed(options.Name, parsed.Hostname(), options.AllowedHosts); err != nil {
			return nil, err
		}
		base = parsed
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	client := &Client{
		name:        options.Name,
		base:        base,
		auth:        options.Auth,
		credentials: options.Credentials,
		allowed:     slices.Clone(options.AllowedHosts),
		metrics:     options.Metrics,
		limiter:     NewLimiter(options.Policy.RequestsPerSecond, options.Policy.Burst, now),
		breaker: NewBreaker(options.Policy.FailureThreshold, options.Policy.Cooldown, now,
			func(state State) { options.Metrics.ObserveBreaker(options.Name, state) }),
		policy: options.Policy,
	}
	transport := options.Transport
	if transport == nil {
		transport = safeTransport()
	}
	client.http = &http.Client{
		Transport: transport,
		Timeout:   options.Policy.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectRefused
		},
	}
	return client, nil
}

// Name returns the Connector name.
func (c *Client) Name() string { return c.name }

// MaxBytes returns the response byte cap.
func (c *Client) MaxBytes() int64 { return c.policy.MaxBytes }

// MaxPages returns the page cap.
func (c *Client) MaxPages() int { return c.policy.MaxPages }

// Credentials returns the credential resolver.
func (c *Client) Credentials() Credentials { return c.credentials }

// Secret returns one credential value named by the Connector's Auth reference.
func (c *Client) Secret(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("connector %s: no credential key is configured", c.name)
	}
	if c.credentials == nil {
		return "", fmt.Errorf("connector %s: %w", c.name, ErrMissingCredential)
	}
	value, err := c.credentials.Get(ctx, c.auth.SecretName, key)
	if err != nil {
		return "", fmt.Errorf("connector %s: %w", c.name, err)
	}
	return value, nil
}

// Get performs a GET against a path on the base URL.
func (c *Client) Get(ctx context.Context, path string, query url.Values) (Response, error) {
	return c.do(ctx, http.MethodGet, c.resolve(path, query), nil, nil, "")
}

// GetWithHeaders performs a GET with extra request headers, such as a tenant.
func (c *Client) GetWithHeaders(
	ctx context.Context, path string, query url.Values, headers map[string]string,
) (Response, error) {
	return c.do(ctx, http.MethodGet, c.resolve(path, query), nil, headers, "")
}

// Post performs a POST against a path on the base URL. POST is used only for
// read-shaped backend APIs, never for a mutation.
func (c *Client) Post(
	ctx context.Context, path string, query url.Values, body []byte, contentType string, headers map[string]string,
) (Response, error) {
	return c.do(ctx, http.MethodPost, c.resolve(path, query), body, headers, contentType)
}

// Do performs a request against an absolute URL. It is used by backends whose
// endpoint is regional, such as CloudWatch.
func (c *Client) Do(
	ctx context.Context, method, absoluteURL string, body []byte, contentType string, headers map[string]string,
) (Response, error) {
	parsed, err := url.Parse(absoluteURL)
	if err != nil {
		return Response{}, fmt.Errorf("connector %s: %w", c.name, err)
	}
	if err := checkHostAllowed(c.name, parsed.Hostname(), c.allowed); err != nil {
		return Response{}, err
	}
	return c.do(ctx, method, absoluteURL, body, headers, contentType)
}

// resolve joins a path onto the base URL.
func (c *Client) resolve(path string, query url.Values) string {
	if c.base == nil {
		return path
	}
	resolved := strings.TrimSuffix(c.base.String(), "/") + "/" + strings.TrimPrefix(path, "/")
	parsed, err := url.Parse(resolved)
	if err != nil {
		return resolved
	}
	values := parsed.Query()
	for key, entries := range query {
		for _, entry := range entries {
			values.Add(key, entry)
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

// do executes one guarded call, retrying a server error once.
func (c *Client) do(
	ctx context.Context, method, target string, body []byte, headers map[string]string, contentType string,
) (Response, error) {
	if method != http.MethodGet && method != http.MethodPost {
		return Response{}, fmt.Errorf("connector %s: only GET and POST are permitted", c.name)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return Response{}, fmt.Errorf("connector %s: build request: %w", c.name, err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return c.perform(ctx, request)
}

// DoRequest performs a prepared request, such as a SigV4-signed CloudWatch
// call, through the same rate limit, breaker, retry and byte cap.
func (c *Client) DoRequest(ctx context.Context, request *http.Request) (Response, error) {
	if err := checkHostAllowed(c.name, request.URL.Hostname(), c.allowed); err != nil {
		return Response{}, err
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		return Response{}, fmt.Errorf("connector %s: only GET and POST are permitted", c.name)
	}
	return c.perform(ctx, request)
}

// perform applies the guard and the retry policy to a prepared request.
func (c *Client) perform(ctx context.Context, request *http.Request) (Response, error) {
	if !c.limiter.Take() {
		c.metrics.ObserveError(c.name, "rate_limited")
		c.metrics.ObserveRequest(c.name, "rate_limited", 0)
		return Response{}, fmt.Errorf("connector %s: %w", c.name, ErrRateLimited)
	}
	if !c.breaker.Allow() {
		c.metrics.ObserveError(c.name, "breaker_open")
		c.metrics.ObserveRequest(c.name, "breaker_open", 0)
		return Response{}, fmt.Errorf("connector %s: %w", c.name, ErrBreakerOpen)
	}
	started := time.Now()
	response, err := c.execute(ctx, request)
	if err == nil && httpStatusIsServerError(response.Status) {
		// One retry on a server error. Every Connector call is read-shaped, so
		// repeating it cannot duplicate a side effect.
		response, err = c.execute(ctx, request)
	}
	if err == nil && (response.Status < 200 || response.Status > 299) {
		err = &StatusError{Status: response.Status, Body: string(response.Body)}
	}
	c.breaker.Record(err)
	elapsed := time.Since(started)
	outcome := "success"
	if err != nil {
		outcome = "error"
		c.metrics.ObserveError(c.name, errorReason(err))
	}
	c.metrics.ObserveRequest(c.name, outcome, elapsed)
	return response, err
}

// execute performs one attempt and reads a bounded body.
func (c *Client) execute(ctx context.Context, prepared *http.Request) (Response, error) {
	request := prepared.Clone(ctx)
	if prepared.GetBody != nil {
		body, err := prepared.GetBody()
		if err != nil {
			return Response{}, fmt.Errorf("connector %s: replay the request body: %w", c.name, err)
		}
		request.Body = body
	}
	if request.Header.Get("Authorization") == "" {
		authHeaders, err := c.authHeaders(ctx)
		if err != nil {
			return Response{}, err
		}
		for key, value := range authHeaders {
			request.Header.Set(key, value)
		}
	}
	response, err := c.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrRedirectRefused) {
			return Response{}, fmt.Errorf("connector %s: %w", c.name, err)
		}
		return Response{}, fmt.Errorf("connector %s: %w", c.name, err)
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, c.policy.MaxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return Response{}, fmt.Errorf("connector %s: read response: %w", c.name, err)
	}
	truncated := int64(len(raw)) > c.policy.MaxBytes
	if truncated {
		raw = raw[:c.policy.MaxBytes]
	}
	return Response{Status: response.StatusCode, Header: response.Header, Body: raw, Truncated: truncated}, nil
}

// authHeaders returns the headers that the configured reference implies.
func (c *Client) authHeaders(ctx context.Context) (map[string]string, error) {
	headers := make(map[string]string, 2)
	if c.auth.TokenKey != "" {
		token, err := c.Secret(ctx, c.auth.TokenKey)
		if err != nil {
			return nil, err
		}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
	}
	if c.auth.UsernameKey != "" && c.auth.PasswordKey != "" {
		username, err := c.Secret(ctx, c.auth.UsernameKey)
		if err != nil {
			return nil, err
		}
		password, err := c.Secret(ctx, c.auth.PasswordKey)
		if err != nil {
			return nil, err
		}
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	}
	return headers, nil
}

// safeTransport returns a transport that refuses link-local and metadata
// addresses at connect time, so a hostname that resolves to metadata cannot
// smuggle a request through. Proxy environment variables are ignored on
// purpose: a proxy would defeat the address check.
func safeTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		return checkAddress(host)
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 0,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
}

// checkAddress refuses a link-local or metadata IP address.
func checkAddress(host string) error {
	address, err := netip.ParseAddr(strings.SplitN(host, "%", 2)[0])
	if err != nil {
		return fmt.Errorf("resolve connector address %q: %w", host, err)
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return fmt.Errorf("connector address %s is link-local or a cloud metadata endpoint", address)
		}
	}
	return nil
}

// checkHostAllowed enforces the optional host allowlist and the hostile-name
// list.
func checkHostAllowed(name, host string, allowed []string) error {
	lower := strings.ToLower(host)
	if slices.Contains(blockedHosts, lower) {
		return fmt.Errorf("connector %s: host %q is a cloud metadata endpoint", name, host)
	}
	if len(allowed) == 0 {
		return nil
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, lower) {
			return nil
		}
	}
	return fmt.Errorf("connector %s: host %q is not in connectors.allowedHosts", name, host)
}

// errorReason classifies a failure for the error metric.
func errorReason(err error) string {
	switch {
	case errors.Is(err, ErrBreakerOpen):
		return "breaker_open"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrRedirectRefused):
		return "redirect"
	default:
		if status, ok := errors.AsType[*StatusError](err); ok {
			if status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
				return "unauthorized"
			}
			return "status"
		}
		return "backend"
	}
}

// truncateText bounds an error string.
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

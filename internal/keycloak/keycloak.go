// Package keycloak answers authorization questions against the realm that
// issues the dashboard's identities. The admission webhook uses it to decide
// whether an approver that pagerless-server asserted is a member of the
// approvers group, independently of what pagerless-server claims (ADR-0020).
package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultGroup is the realm group whose members may approve through the dashboard.
	DefaultGroup = "pagerless-approvers"
	// queryTrue is the admin API's value for the boolean query parameters this package sends.
	queryTrue = "true"
	// defaultCacheTTL bounds how long a membership answer is reused, so a removal from the group takes effect
	// within this window without asking Keycloak on every admission.
	defaultCacheTTL = 30 * time.Second
	// defaultTimeout bounds one Keycloak round trip. Admission waits for it: a slow or unavailable Keycloak must
	// fail the decision closed rather than hold the API server's admission path open.
	defaultTimeout = 5 * time.Second
	// tokenMargin is how long before expiry a cached service-account token is replaced.
	tokenMargin = 30 * time.Second
	// maxResponseBytes bounds a Keycloak response body. The admin API answers with small JSON documents.
	maxResponseBytes = 1 << 20
)

// Config configures the Keycloak admin client.
type Config struct {
	// BaseURL is the Keycloak base URL, such as http://keycloak.keycloak.svc.cluster.local:8080.
	BaseURL string
	// Realm is the realm that holds the users and groups, such as pagerless.
	Realm string
	// ClientID is the confidential client that requests the admin token.
	ClientID string
	// ClientSecret returns the client secret. It is read on every token request so that a rotated Secret
	// takes effect without a restart.
	ClientSecret func() (string, error)
	// Group is the group whose members may approve. It defaults to DefaultGroup.
	Group string
	// HTTPClient performs the requests. It defaults to a client with defaultTimeout.
	HTTPClient *http.Client
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
	// CacheTTL is how long a membership answer is reused. It defaults to defaultCacheTTL.
	CacheTTL time.Duration
}

// Membership reports whether a Keycloak user may approve. It is safe for concurrent use.
type Membership struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret func() (string, error)
	group        string
	client       *http.Client
	now          func() time.Time
	cacheTTL     time.Duration

	mu       sync.Mutex
	token    string
	tokenEnd time.Time
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	approver bool
	until    time.Time
}

// New returns a Membership. It performs no network request: Keycloak is asked
// on the first decision, so a server can start before the identity provider is
// reachable.
func New(cfg Config) (*Membership, error) {
	switch {
	case cfg.BaseURL == "":
		return nil, errors.New("a Keycloak base URL is required")
	case cfg.Realm == "":
		return nil, errors.New("a Keycloak realm is required")
	case cfg.ClientID == "":
		return nil, errors.New("a Keycloak client ID is required")
	case cfg.ClientSecret == nil:
		return nil, errors.New("a Keycloak client secret source is required")
	}
	group := cfg.Group
	if group == "" {
		group = DefaultGroup
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = defaultCacheTTL
	}
	return &Membership{
		baseURL:      cfg.BaseURL,
		realm:        cfg.Realm,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		group:        group,
		client:       client,
		now:          now,
		cacheTTL:     ttl,
		cache:        make(map[string]cacheEntry),
	}, nil
}

// Group returns the group whose members may approve.
func (m *Membership) Group() string {
	return m.group
}

// IsApprover reports whether the named user belongs to the approvers group. A
// user who does not exist, or whose name is ambiguous, is not an approver. An
// error means Keycloak could not answer; the caller must fail closed.
func (m *Membership) IsApprover(ctx context.Context, username string) (bool, error) {
	if username == "" {
		return false, nil
	}
	now := m.now()
	m.mu.Lock()
	entry, found := m.cache[username]
	m.mu.Unlock()
	if found && now.Before(entry.until) {
		return entry.approver, nil
	}

	id, err := m.userID(ctx, username)
	if err != nil {
		return false, err
	}
	approver := false
	if id != "" {
		approver, err = m.inGroup(ctx, id)
		if err != nil {
			return false, err
		}
	}
	m.mu.Lock()
	m.cache[username] = cacheEntry{approver: approver, until: now.Add(m.cacheTTL)}
	m.mu.Unlock()
	return approver, nil
}

// userID resolves a username to its Keycloak subject. It returns an empty ID
// when no user has exactly that username.
func (m *Membership) userID(ctx context.Context, username string) (string, error) {
	query := url.Values{"username": {username}, "exact": {queryTrue}, "briefRepresentation": {queryTrue}}
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := m.adminGet(ctx, "/users?"+query.Encode(), &users); err != nil {
		return "", err
	}
	switch len(users) {
	case 0:
		return "", nil
	case 1:
		return users[0].ID, nil
	default:
		// Keycloak treats usernames as unique per realm. More than one match means the answer is not a
		// single unambiguous user, so no member may be asserted.
		return "", nil
	}
}

// inGroup reports whether the user belongs to the approvers group.
func (m *Membership) inGroup(ctx context.Context, userID string) (bool, error) {
	query := url.Values{"briefRepresentation": {queryTrue}}
	var groups []struct {
		Name string `json:"name"`
	}
	path := "/users/" + url.PathEscape(userID) + "/groups?" + query.Encode()
	if err := m.adminGet(ctx, path, &groups); err != nil {
		return false, err
	}
	for _, group := range groups {
		if group.Name == m.group {
			return true, nil
		}
	}
	return false, nil
}

// adminGet performs one admin request with a service-account token.
func (m *Membership) adminGet(ctx context.Context, path string, into any) error {
	token, err := m.adminToken(ctx)
	if err != nil {
		return err
	}
	target := m.baseURL + "/admin/realms/" + url.PathEscape(m.realm) + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build the Keycloak request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("ask Keycloak: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("keycloak answered GET %s with HTTP %d", path, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read the Keycloak response: %w", err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode the Keycloak response: %w", err)
	}
	return nil
}

// adminToken returns a cached service-account token, requesting a new one when
// the cached token is close to expiry.
func (m *Membership) adminToken(ctx context.Context) (string, error) {
	now := m.now()
	m.mu.Lock()
	if m.token != "" && now.Before(m.tokenEnd) {
		token := m.token
		m.mu.Unlock()
		return token, nil
	}
	m.mu.Unlock()

	secret, err := m.clientSecret()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {m.clientID},
		"client_secret": {secret},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/realms/"+url.PathEscape(m.realm)+"/protocol/openid-connect/token", nil)
	if err != nil {
		return "", fmt.Errorf("build the Keycloak token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Body = io.NopCloser(strings.NewReader(form.Encode()))
	response, err := m.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request a Keycloak admin token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the Keycloak token request answered with HTTP %d", response.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read the Keycloak token response: %w", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("decode the Keycloak token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", errors.New("the Keycloak response carried no access token")
	}
	expires := now.Add(time.Duration(body.ExpiresIn) * time.Second)
	if margin := expires.Add(-tokenMargin); margin.After(now) {
		expires = margin
	}
	m.mu.Lock()
	m.token, m.tokenEnd = body.AccessToken, expires
	m.mu.Unlock()
	return body.AccessToken, nil
}

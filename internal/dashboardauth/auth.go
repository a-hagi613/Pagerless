// Package dashboardauth signs dashboard users in through the Keycloak realm
// that issues their identity. The server performs the OpenID Connect
// authorization-code flow with PKCE, verifies the ID token, and keeps the
// tokens server-side: the browser holds only an opaque session cookie
// (ADR-0020).
package dashboardauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// sessionCookieName holds the opaque session identifier. It carries no identity.
	sessionCookieName = "pagerless_session"
	// flowCookieName holds the state, nonce, and PKCE verifier of one sign-in attempt.
	flowCookieName = "pagerless_oidc"
	// flowLifetime bounds how long a sign-in attempt may take.
	flowLifetime = 10 * time.Minute
	// DefaultSessionTTL is how long a signed-in session lasts before the user must sign in again.
	DefaultSessionTTL = 8 * time.Hour
	// tokenBytes is the entropy of a session identifier, CSRF token, state, or nonce.
	tokenBytes = 32
)

// Config configures the Keycloak sign-in.
type Config struct {
	// Issuer is the realm issuer, such as http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless.
	Issuer string
	// ClientID is the confidential dashboard client.
	ClientID string
	// ClientSecret returns the client secret. It is read on every token request so that a rotated Secret takes
	// effect without a restart.
	ClientSecret func() (string, error)
	// RedirectURL is the absolute callback URL that the browser uses, such as
	// http://localhost:8080/auth/callback.
	RedirectURL string
	// ApproversGroup is the realm group whose members may approve. It defaults to pagerless-approvers.
	ApproversGroup string
	// SessionTTL is how long a session lasts. It defaults to DefaultSessionTTL.
	SessionTTL time.Duration
	// SecureCookies marks the cookies Secure. It must be false only where the browser reaches the dashboard
	// over plain HTTP, such as a local port-forward.
	SecureCookies bool
	// HTTPClient performs discovery and token requests. It defaults to http.DefaultClient.
	HTTPClient *http.Client
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
}

// User is the signed-in dashboard user. It is derived from the verified ID token.
type User struct {
	// Subject is the stable Keycloak subject identifier.
	Subject string `json:"subject"`
	// Username is the Keycloak preferred username, which is recorded as the approver.
	Username string `json:"username"`
	// Groups are the realm groups that the token carries.
	Groups []string `json:"groups"`
	// Approver reports whether the user belongs to the approvers group.
	Approver bool `json:"canApprove"`
	// CSRF is the token that a state-changing request must repeat in the X-CSRF-Token header.
	CSRF string `json:"csrfToken,omitempty"`
	// ExpiresAt is when the session ends.
	ExpiresAt time.Time `json:"expiresAt"`
}

// Auth performs the Keycloak flow and resolves session cookies.
type Auth struct {
	issuer       string
	issuerBase   string
	realmPath    string
	clientID     string
	clientSecret func() (string, error)
	redirectURL  string
	browserBase  string
	group        string
	sessionTTL   time.Duration
	secure       bool
	client       *http.Client
	now          func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier

	sessions sessionStore
}

// New returns an Auth. It performs no network request: the realm is discovered
// on the first sign-in, so the server can start before Keycloak is reachable.
func New(cfg Config) (*Auth, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("an OIDC issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("an OIDC client ID is required")
	case cfg.ClientSecret == nil:
		return nil, errors.New("an OIDC client secret source is required")
	case cfg.RedirectURL == "":
		return nil, errors.New("an OIDC redirect URL is required")
	}
	issuerURL, err := url.Parse(cfg.Issuer)
	if err != nil || issuerURL.Scheme == "" || issuerURL.Host == "" {
		return nil, fmt.Errorf("the OIDC issuer URL is invalid: %w", err)
	}
	redirectURL, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirectURL.Scheme == "" || redirectURL.Host == "" {
		return nil, fmt.Errorf("the OIDC redirect URL is invalid: %w", err)
	}
	group := cfg.ApproversGroup
	if group == "" {
		group = "pagerless-approvers"
	}
	ttl := cfg.SessionTTL
	if ttl == 0 {
		ttl = DefaultSessionTTL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Auth{
		issuer:       cfg.Issuer,
		issuerBase:   issuerURL.Scheme + "://" + issuerURL.Host,
		realmPath:    strings.TrimSuffix(issuerURL.Path, "/"),
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURL:  cfg.RedirectURL,
		browserBase:  redirectURL.Scheme + "://" + redirectURL.Host,
		group:        group,
		sessionTTL:   ttl,
		secure:       cfg.SecureCookies,
		client:       client,
		now:          now,
		sessions:     newSessionStore(now),
	}, nil
}

// Group returns the group whose members may approve.
func (a *Auth) Group() string {
	return a.group
}

// User returns the signed-in user of the request, or nil when the request
// carries no live session.
func (a *Auth) User(r *http.Request) *User {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	session, ok := a.sessions.get(cookie.Value)
	if !ok {
		return nil
	}
	return &User{
		Subject:   session.subject,
		Username:  session.username,
		Groups:    append([]string(nil), session.groups...),
		Approver:  contains(session.groups, a.group),
		CSRF:      session.csrf,
		ExpiresAt: session.expiresAt,
	}
}

// SignIn starts the authorization-code flow and redirects the browser to Keycloak.
func (a *Auth) SignIn() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		state, err := randomToken()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		nonce, err := randomToken()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		verifier := oauth2.GenerateVerifier()
		a.setFlowCookie(w, flow{State: state, Nonce: nonce, Verifier: verifier})
		oauth, err := a.oauth(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		target := oauth.AuthCodeURL(
			state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
		http.Redirect(w, r, a.browserAuthURL(target), http.StatusFound)
	})
}

// Callback completes the flow: it checks the state, exchanges the code, and
// verifies the ID token's signature, issuer, audience, expiry, and nonce before
// it records a session.
func (a *Auth) Callback() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pending, ok := a.flowCookie(r)
		a.clearFlowCookie(w)
		if !ok {
			a.fail(w, r, errors.New("the sign-in attempt cookie is missing or unreadable"))
			return
		}
		query := r.URL.Query()
		if query.Get("error") != "" {
			a.fail(w, r, errors.New("keycloak refused the sign-in"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(pending.State)) != 1 {
			a.fail(w, r, errors.New("the sign-in state does not match"))
			return
		}
		ctx := oidc.ClientContext(r.Context(), a.client)
		oauth, err := a.oauth(ctx)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		token, err := oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(pending.Verifier))
		if err != nil {
			a.fail(w, r, fmt.Errorf("exchange the authorization code: %w", err))
			return
		}
		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok || rawIDToken == "" {
			a.fail(w, r, errors.New("the token response carries no ID token"))
			return
		}
		verifier, err := a.idTokenVerifier(ctx)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			a.fail(w, r, fmt.Errorf("verify the ID token: %w", err))
			return
		}
		if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(pending.Nonce)) != 1 {
			a.fail(w, r, errors.New("the ID token nonce does not match"))
			return
		}
		var claims struct {
			PreferredUsername string   `json:"preferred_username"`
			Email             string   `json:"email"`
			Groups            []string `json:"groups"`
		}
		if err := idToken.Claims(&claims); err != nil {
			a.fail(w, r, fmt.Errorf("read the ID token claims: %w", err))
			return
		}
		username := claims.PreferredUsername
		if username == "" {
			username = claims.Email
		}
		if username == "" {
			username = idToken.Subject
		}
		sessionID, err := randomToken()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		csrf, err := randomToken()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		a.sessions.create(session{
			id:        sessionID,
			subject:   idToken.Subject,
			username:  username,
			groups:    claims.Groups,
			csrf:      csrf,
			expiresAt: a.now().Add(a.sessionTTL),
		})
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   a.secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(a.sessionTTL.Seconds()),
		})
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// SignOut ends the session. It requires the CSRF token, because it changes
// state, and it redirects to a page that starts no new sign-in.
func (a *Auth) SignOut() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		user := a.User(r)
		if user == nil {
			a.clearSessionCookie(w)
			http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(user.CSRF)) != 1 {
			http.Error(w, "the CSRF token is missing or wrong", http.StatusForbidden)
			return
		}
		if cookie, err := r.Cookie(sessionCookieName); err == nil {
			a.sessions.delete(cookie.Value)
		}
		a.clearSessionCookie(w)
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/signed-out", http.StatusSeeOther)
	})
}

// oauth returns the OAuth client configuration for the realm.
func (a *Auth) oauth(ctx context.Context) (*oauth2.Config, error) {
	provider, err := a.realm(ctx)
	if err != nil {
		return nil, err
	}
	secret, err := a.clientSecret()
	if err != nil {
		return nil, err
	}
	return &oauth2.Config{
		ClientID:     a.clientID,
		ClientSecret: secret,
		RedirectURL:  a.redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}, nil
}

// realm discovers the issuer once and remembers the provider.
func (a *Auth) realm(ctx context.Context) (*oidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return a.provider, nil
	}
	ctx = oidc.ClientContext(ctx, a.client)
	provider, err := oidc.NewProvider(ctx, a.issuer)
	if err != nil {
		return nil, fmt.Errorf("discover the OIDC issuer %s: %w", a.issuer, err)
	}
	a.provider = provider
	a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
	return provider, nil
}

// idTokenVerifier returns the realm's ID token verifier.
func (a *Auth) idTokenVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	if _, err := a.realm(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verifier, nil
}

// flow is one pending sign-in attempt. The fields are exported so that the
// cookie payload encodes; they are opaque to the browser.
type flow struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
}

// setFlowCookie records the pending attempt in a short-lived, HTTP-only cookie.
// The values are opaque and checked against the callback, so the cookie needs
// no signature: a tampered cookie cannot produce a valid ID token for the
// server's client.
func (a *Auth) setFlowCookie(w http.ResponseWriter, pending flow) {
	payload, err := json.Marshal(pending)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookieName,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     "/auth",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(flowLifetime.Seconds()),
	})
}

func (a *Auth) flowCookie(r *http.Request) (flow, bool) {
	cookie, err := r.Cookie(flowCookieName)
	if err != nil || cookie.Value == "" {
		return flow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return flow{}, false
	}
	var pending flow
	if err := json.Unmarshal(raw, &pending); err != nil {
		return flow{}, false
	}
	if pending.State == "" || pending.Nonce == "" || pending.Verifier == "" {
		return flow{}, false
	}
	return pending, true
}

func (a *Auth) clearFlowCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: flowCookieName, Value: "", Path: "/auth", HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (a *Auth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// fail answers a sign-in that cannot complete. The browser is told nothing
// beyond that it is not signed in, while the operator gets the reason. Keycloak
// error bodies can name users, so only the wrapped error is logged.
func (a *Auth) fail(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	logf.FromContext(r.Context()).Error(err, "dashboard sign-in failed")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/signed-out?failed=1", http.StatusSeeOther)
}

// session is one signed-in browser.
type session struct {
	id        string
	subject   string
	username  string
	groups    []string
	csrf      string
	expiresAt time.Time
}

// sessionStore holds sessions in memory. pagerless-server runs one replica
// (ADR-0001), and a restart ends every session, which is acceptable for an
// eight-hour human session.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	now      func() time.Time
}

func newSessionStore(now func() time.Time) sessionStore {
	return sessionStore{sessions: make(map[string]session), now: now}
}

func (s *sessionStore) create(value session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, existing := range s.sessions {
		if !now.Before(existing.expiresAt) {
			delete(s.sessions, id)
		}
	}
	s.sessions[value.id] = value
}

func (s *sessionStore) get(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[id]
	if !ok {
		return session{}, false
	}
	if !s.now().Before(value.expiresAt) {
		delete(s.sessions, id)
		return session{}, false
	}
	return value, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// randomToken returns a URL-safe random token.
func randomToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// contains reports whether the list holds the value, comparing case-sensitively as Keycloak group names are.
func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

// Package dashboardauth authenticates Portal and API requests without an
// external identity provider (ADR-0032). In token mode, the operator exchanges
// the bootstrap token from pagerless-admin-secret for an HMAC-SHA256 signed
// session cookie keyed by pagerless-session-key, and logout records the
// session's jti in SQLite so a copied cookie cannot be replayed. In disabled
// mode, every request resolves to the configured proxy identity header or to
// the synthetic user anonymous.
package dashboardauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ModeToken authenticates requests with the bootstrap token and an HMAC-signed cookie.
	ModeToken = "token"
	// ModeDisabled authenticates every request as anonymous or the proxy identity header.
	ModeDisabled = "disabled"

	// DefaultCookieName is the default name of the signed session cookie.
	DefaultCookieName = "pagerless_session"
	// DefaultSessionTTL is how long a signed session cookie remains valid.
	DefaultSessionTTL = 8 * time.Hour
	// DefaultLoginMaxAttempts is the per-address failure ceiling inside DefaultLoginWindow.
	DefaultLoginMaxAttempts = 5
	// DefaultLoginWindow is the sliding window over which login failures are counted.
	DefaultLoginWindow = 5 * time.Minute
	// DefaultLoginLockout is how long a locked-out source address receives 429 responses.
	DefaultLoginLockout = 5 * time.Minute
	// DefaultLoginDelay is the fixed delay applied to every login attempt.
	DefaultLoginDelay = 200 * time.Millisecond

	// MinSessionKeyBytes is the minimum length in bytes of the HMAC-SHA256 signing key.
	MinSessionKeyBytes = 32
	// CookieVersion is the wire format version of the signed session payload.
	CookieVersion = 1
	// claimEntropyBytes is the random byte length of the jti and csrf claims.
	claimEntropyBytes = 16
	// maxLoginBodyBytes bounds the JSON payload of POST /api/auth/login.
	maxLoginBodyBytes = 4096
	// approversGroupName is the group that every authenticated operator holds.
	approversGroupName = "pagerless-approvers"
	// adminSubject is the subject and username of the single operator.
	adminSubject = "admin"
)

// RevocationStore records and queries revoked session identifiers (jti).
// *history.Store implements this interface backed by SQLite.
type RevocationStore interface {
	RevokeSession(ctx context.Context, jti string, now, expiresAt time.Time) error
	IsSessionRevoked(ctx context.Context, jti string, now time.Time) (bool, error)
}

// Config configures Portal and API authentication.
type Config struct {
	// Mode is "token" (default) or "disabled".
	Mode string
	// AdminToken reads the current bootstrap token from pagerless-admin-secret.
	// It is invoked on every login or bearer check so that rotating or deleting
	// the Secret takes effect without restarting the server.
	AdminToken func() (string, error)
	// SessionKey reads the current HMAC signing key from pagerless-session-key.
	// Rotating the Secret invalidates all active cookies immediately.
	SessionKey func() ([]byte, error)
	// CookieName is the session cookie name. It defaults to DefaultCookieName.
	CookieName string
	// SessionTTL is the lifetime of a signed session cookie. It defaults to DefaultSessionTTL.
	SessionTTL time.Duration
	// BaseURL is the browser-facing base URL, used to derive the Secure attribute when CookieSecure is empty.
	BaseURL string
	// CookieSecure is "", "true", or "false". When empty, Secure is derived from BaseURL (true for https://).
	CookieSecure string
	// SecureCookies marks the cookie Secure when CookieSecure and BaseURL are both unset.
	SecureCookies bool
	// BearerToken enables Authorization: Bearer <admin-token> on /api/* requests.
	BearerToken bool
	// IdentityHeader is the trusted reverse-proxy header (such as X-Auth-Request-User)
	// used to attribute actions when Mode is "disabled".
	IdentityHeader string
	// LoginMaxAttempts is the number of failed attempts per source address before lockout.
	LoginMaxAttempts int
	// LoginWindow is the time window over which failed attempts are counted.
	LoginWindow time.Duration
	// LoginLockout is the duration of the per-address lockout.
	LoginLockout time.Duration
	// LoginDelay is the fixed delay per login attempt. It defaults to DefaultLoginDelay.
	LoginDelay time.Duration
	// Revocations stores revoked session identifiers (jti). When nil, an in-memory fallback is used.
	Revocations RevocationStore
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
	// Sleep pauses for the fixed login delay. It defaults to time.Sleep.
	Sleep func(time.Duration)
	// Logger logs lockout warnings. It defaults to slog.Default().
	Logger *slog.Logger
}

// User is the authenticated Portal or API caller.
type User struct {
	Subject   string    `json:"subject"`
	Username  string    `json:"username"`
	Groups    []string  `json:"groups,omitempty"`
	Approver  bool      `json:"canApprove"`
	CSRF      string    `json:"csrfToken"`
	ExpiresAt time.Time `json:"expiresAt"`
	Mode      string    `json:"mode"`
	JTI       string    `json:"-"`
	Bearer    bool      `json:"-"`
}

// signedClaims is the JSON payload inside the signed session cookie.
type signedClaims struct {
	Version   int    `json:"v"`
	Subject   string `json:"sub"`
	JTI       string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	CSRF      string `json:"csrf"`
}

// Auth authenticates HTTP requests and serves login/logout handlers.
type Auth struct {
	mode           string
	adminToken     func() (string, error)
	sessionKey     func() ([]byte, error)
	cookieName     string
	sessionTTL     time.Duration
	secure         bool
	bearerToken    bool
	identityHeader string
	maxAttempts    int
	loginWindow    time.Duration
	loginLockout   time.Duration
	loginDelay     time.Duration
	revocations    RevocationStore
	now            func() time.Time
	sleep          func(time.Duration)
	logger         *slog.Logger

	mu       sync.Mutex
	attempts map[string]*ipAttempts
}

type ipAttempts struct {
	failures    []time.Time
	lockedUntil time.Time
}

// New validates cfg and returns an Auth instance.
func New(cfg Config) (*Auth, error) {
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		mode = ModeToken
	}
	if mode != ModeToken && mode != ModeDisabled {
		return nil, fmt.Errorf("invalid auth.mode %q: must be %q or %q", cfg.Mode, ModeToken, ModeDisabled)
	}

	secure, err := resolveSecureCookie(cfg.CookieSecure, cfg.BaseURL, cfg.SecureCookies)
	if err != nil {
		return nil, err
	}

	if mode == ModeToken {
		if cfg.AdminToken == nil {
			return nil, errors.New("no admin token source configured: provide AdminToken or set auth.mode=disabled")
		}
		tok, err := cfg.AdminToken()
		if err != nil || strings.TrimSpace(tok) == "" {
			return nil, fmt.Errorf("no admin token: create Secret pagerless-admin-secret or set auth.mode=disabled")
		}
		if cfg.SessionKey == nil {
			return nil, errors.New("no session key source configured: create Secret pagerless-session-key")
		}
		key, err := cfg.SessionKey()
		if err != nil {
			return nil, fmt.Errorf("read session signing key: %w", err)
		}
		if len(bytesTrimmed(key)) < MinSessionKeyBytes {
			return nil, fmt.Errorf("session signing key must be at least %d bytes", MinSessionKeyBytes)
		}
	}

	cookieName := strings.TrimSpace(cfg.CookieName)
	if cookieName == "" {
		cookieName = DefaultCookieName
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	maxAttempts := cfg.LoginMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultLoginMaxAttempts
	}
	window := cfg.LoginWindow
	if window <= 0 {
		window = DefaultLoginWindow
	}
	lockout := cfg.LoginLockout
	if lockout <= 0 {
		lockout = DefaultLoginLockout
	}
	delay := cfg.LoginDelay
	if delay <= 0 {
		delay = DefaultLoginDelay
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	revocations := cfg.Revocations
	if revocations == nil {
		revocations = newMemoryRevocationStore()
	}

	return &Auth{
		mode:           mode,
		adminToken:     cfg.AdminToken,
		sessionKey:     cfg.SessionKey,
		cookieName:     cookieName,
		sessionTTL:     ttl,
		secure:         secure,
		bearerToken:    cfg.BearerToken,
		identityHeader: strings.TrimSpace(cfg.IdentityHeader),
		maxAttempts:    maxAttempts,
		loginWindow:    window,
		loginLockout:   lockout,
		loginDelay:     delay,
		revocations:    revocations,
		now:            now,
		sleep:          sleep,
		logger:         logger,
		attempts:       make(map[string]*ipAttempts),
	}, nil
}

// resolveSecureCookie determines whether session cookies carry the Secure flag.
func resolveSecureCookie(explicit, baseURL string, fallback bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "":
		trimmed := strings.ToLower(strings.TrimSpace(baseURL))
		if trimmed != "" {
			return strings.HasPrefix(trimmed, "https://"), nil
		}
		return fallback, nil
	default:
		return false, fmt.Errorf("auth.session.secure must be \"\", \"true\", or \"false\", got %q", explicit)
	}
}

// Mode returns the configured authentication mode ("token" or "disabled").
func (a *Auth) Mode() string {
	return a.mode
}

// CookieName returns the configured session cookie name.
func (a *Auth) CookieName() string {
	return a.cookieName
}

// User resolves the authenticated caller from r, or returns nil when unauthenticated.
func (a *Auth) User(r *http.Request) *User {
	now := a.now()
	if a.mode == ModeDisabled {
		username := "anonymous"
		if a.identityHeader != "" {
			if forwarded := strings.TrimSpace(r.Header.Get(a.identityHeader)); forwarded != "" {
				username = forwarded
			}
		}
		return &User{
			Subject:   username,
			Username:  username,
			Groups:    []string{approversGroupName},
			Approver:  true,
			CSRF:      "",
			ExpiresAt: now.Add(a.sessionTTL),
			Mode:      ModeDisabled,
		}
	}

	// If an Authorization header is present, evaluate Bearer token authentication exclusively
	// and never fall back to a session cookie on failure.
	if authHeader := strings.TrimSpace(r.Header.Get("Authorization")); authHeader != "" {
		if !a.bearerToken {
			return nil
		}
		const prefix = "bearer "
		if len(authHeader) <= len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
			return nil
		}
		supplied := strings.TrimSpace(authHeader[len(prefix):])
		expected, err := a.readAdminToken()
		if err != nil || !constantTimeTokenEqual(supplied, expected) {
			return nil
		}
		return &User{
			Subject:   adminSubject,
			Username:  adminSubject,
			Groups:    []string{approversGroupName},
			Approver:  true,
			CSRF:      "",
			ExpiresAt: now.Add(a.sessionTTL),
			Mode:      ModeToken,
			Bearer:    true,
		}
	}

	cookie, err := r.Cookie(a.cookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	claims, ok := a.verifyCookie(r.Context(), cookie.Value, now)
	if !ok {
		return nil
	}
	return &User{
		Subject:   claims.Subject,
		Username:  claims.Subject,
		Groups:    []string{approversGroupName},
		Approver:  true,
		CSRF:      claims.CSRF,
		ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
		Mode:      ModeToken,
		JTI:       claims.JTI,
	}
}

// ValidateCSRF checks whether the state-changing request r is authorized for user.
// CSRF is skipped when auth.mode is disabled or when the request authenticated
// via a valid Bearer token.
func (a *Auth) ValidateCSRF(r *http.Request, user *User) bool {
	if user == nil {
		return false
	}
	if a.mode == ModeDisabled || user.Bearer {
		return true
	}
	supplied := r.Header.Get("X-CSRF-Token")
	if supplied == "" || user.CSRF == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(user.CSRF)) == 1
}

// Login handles POST /api/auth/login.
func (a *Auth) Login() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.Header().Set("Cache-Control", "no-store")

		if a.mode == ModeDisabled {
			writeAuthError(w, http.StatusForbidden, "login_disabled", "token sign-in is disabled when auth.mode is disabled")
			return
		}

		now := a.now()
		ip := a.clientIP(r)
		if locked, retryAfter := a.isLockedOut(ip, now); locked {
			a.sleep(a.loginDelay)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			a.warnLockout(r.Context(), ip, retryAfter)
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]string{
					"code":    "too_many_attempts",
					"message": "too many failed sign-in attempts; wait before retrying",
				},
				"retryAfter": retryAfter,
			})
			return
		}

		var body struct {
			Token string `json:"token"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeAuthError(w, http.StatusBadRequest, "bad_request", "malformed JSON login request")
			return
		}

		submitted := strings.TrimSpace(body.Token)
		expected, tokenErr := a.readAdminToken()
		matched := constantTimeTokenEqual(submitted, expected) && tokenErr == nil
		a.sleep(a.loginDelay)

		if !matched {
			if lockedNow, retryAfter := a.recordFailure(ip, now); lockedNow {
				a.warnLockout(r.Context(), ip, retryAfter)
			}
			writeAuthError(w, http.StatusUnauthorized, "invalid_token", "the bootstrap token was not accepted")
			return
		}

		a.clearFailures(ip)

		jti, err := randomBase64URL(claimEntropyBytes)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal_error", "mint session identifier")
			return
		}
		csrf, err := randomBase64URL(claimEntropyBytes)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal_error", "mint CSRF token")
			return
		}
		expiresAt := now.Add(a.sessionTTL).UTC()
		claims := signedClaims{
			Version:   CookieVersion,
			Subject:   adminSubject,
			JTI:       jti,
			IssuedAt:  now.Unix(),
			ExpiresAt: expiresAt.Unix(),
			CSRF:      csrf,
		}
		signedValue, err := a.signClaims(claims)
		if err != nil {
			writeAuthError(w, http.StatusInternalServerError, "internal_error", "sign session cookie")
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     a.cookieName,
			Value:    signedValue,
			Path:     "/",
			HttpOnly: true,
			Secure:   a.secure,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(a.sessionTTL.Seconds()),
		})
		writeJSON(w, http.StatusOK, map[string]string{
			"expiresAt": expiresAt.Format(time.RFC3339),
		})
	})
}

// Logout handles POST /api/auth/logout.
func (a *Auth) Logout() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAuthError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.Header().Set("Cache-Control", "no-store")

		if a.mode == ModeDisabled {
			a.clearSessionCookie(w)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		user := a.User(r)
		if user == nil {
			a.clearSessionCookie(w)
			writeAuthError(w, http.StatusUnauthorized, "unauthenticated", "sign in to use the dashboard")
			return
		}
		if !a.ValidateCSRF(r, user) {
			writeAuthError(w, http.StatusForbidden, "csrf", "the CSRF token is missing or wrong")
			return
		}
		if user.JTI != "" {
			if err := a.revocations.RevokeSession(r.Context(), user.JTI, a.now(), user.ExpiresAt); err != nil {
				logf.FromContext(r.Context()).Error(err, "revoke session in SQLite")
				writeAuthError(w, http.StatusInternalServerError, "internal_error", "could not revoke session")
				return
			}
		}
		a.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
	})
}

// SignIn handles GET /login for browser navigation. In disabled mode it redirects
// to /; in token mode it serves a minimal HTML sign-in fallback when the SPA
// does not intercept /login.
func (a *Auth) SignIn() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.mode == ModeDisabled {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8">`+
			`<title>Pagerless Sign In</title></head><body><div id="root">Sign in to Pagerless</div></body></html>`)
	})
}

// signClaims serializes and HMAC-SHA256 signs the session claims.
func (a *Auth) signClaims(claims signedClaims) (string, error) {
	key, err := a.readSessionKey()
	if err != nil {
		return "", err
	}
	rawPayload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(rawPayload)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encodedPayload))
	encodedMAC := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + encodedMAC, nil
}

// verifyCookie verifies the HMAC-SHA256 signature BEFORE parsing the payload,
// checks claim validity and expiry, and consults the revocation store.
func (a *Auth) verifyCookie(ctx context.Context, raw string, now time.Time) (signedClaims, bool) {
	encodedPayload, encodedMAC, ok := strings.Cut(raw, ".")
	if !ok || encodedPayload == "" || encodedMAC == "" || strings.Contains(encodedMAC, ".") {
		return signedClaims{}, false
	}
	// Strict decoding rejects a non-canonical encoding whose unused trailing
	// bits are not zero, so exactly one string carries one signature.
	suppliedMAC, err := base64.RawURLEncoding.Strict().DecodeString(encodedMAC)
	if err != nil || len(suppliedMAC) != sha256.Size {
		return signedClaims{}, false
	}
	key, err := a.readSessionKey()
	if err != nil {
		return signedClaims{}, false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encodedPayload))
	expectedMAC := mac.Sum(nil)

	// Verify the signature in constant time before decoding or parsing the payload.
	if !hmac.Equal(suppliedMAC, expectedMAC) {
		return signedClaims{}, false
	}

	rawPayload, err := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	if err != nil {
		return signedClaims{}, false
	}
	var claims signedClaims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return signedClaims{}, false
	}
	if claims.Version != CookieVersion || claims.Subject == "" || claims.JTI == "" || claims.CSRF == "" {
		return signedClaims{}, false
	}
	if claims.IssuedAt <= 0 || now.Unix() >= claims.ExpiresAt {
		return signedClaims{}, false
	}
	if a.revocations != nil {
		revoked, err := a.revocations.IsSessionRevoked(ctx, claims.JTI, now)
		if err != nil || revoked {
			return signedClaims{}, false
		}
	}
	return claims, true
}

func (a *Auth) readAdminToken() (string, error) {
	if a.adminToken == nil {
		return "", errors.New("no admin token source")
	}
	raw, err := a.adminToken()
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("admin token is empty")
	}
	return trimmed, nil
}

func (a *Auth) readSessionKey() ([]byte, error) {
	if a.sessionKey == nil {
		return nil, errors.New("no session key source")
	}
	raw, err := a.sessionKey()
	if err != nil {
		return nil, err
	}
	trimmed := bytesTrimmed(raw)
	if len(trimmed) < MinSessionKeyBytes {
		return nil, errors.New("session key is shorter than 32 bytes")
	}
	return trimmed, nil
}

func bytesTrimmed(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

// constantTimeTokenEqual compares two tokens in constant time without leaking
// either token's length or prefix match.
func constantTimeTokenEqual(supplied, expected string) bool {
	suppliedHash := sha256.Sum256([]byte(supplied))
	expectedHash := sha256.Sum256([]byte(expected))
	hashMatch := subtle.ConstantTimeCompare(suppliedHash[:], expectedHash[:])
	lenMatch := subtle.ConstantTimeEq(int32(len(supplied)), int32(len(expected)))
	nonEmpty := 0
	if len(expected) > 0 {
		nonEmpty = 1
	}
	return (hashMatch & lenMatch & nonEmpty) == 1
}

func (a *Auth) clientIP(r *http.Request) string {
	if a.identityHeader != "" {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

func (a *Auth) isLockedOut(ip string, now time.Time) (bool, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.attempts[ip]
	if !ok {
		return false, 0
	}
	if now.Before(entry.lockedUntil) {
		seconds := max(1, int(math.Ceil(entry.lockedUntil.Sub(now).Seconds())))
		return true, seconds
	}
	return false, 0
}

func (a *Auth) recordFailure(ip string, now time.Time) (bool, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.attempts[ip]
	if !ok {
		entry = &ipAttempts{}
		a.attempts[ip] = entry
	}
	cutoff := now.Add(-a.loginWindow)
	kept := entry.failures[:0]
	for _, ts := range entry.failures {
		if !ts.Before(cutoff) {
			kept = append(kept, ts)
		}
	}
	entry.failures = append(kept, now)
	if len(entry.failures) >= a.maxAttempts {
		entry.lockedUntil = now.Add(a.loginLockout)
		entry.failures = nil
		seconds := max(1, int(math.Ceil(a.loginLockout.Seconds())))
		return true, seconds
	}
	return false, 0
}

func (a *Auth) clearFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.attempts, ip)
}

func (a *Auth) warnLockout(ctx context.Context, ip string, retryAfter int) {
	a.logger.WarnContext(ctx, "login rate limit exceeded; source address locked out",
		"remoteAddr", ip, "retryAfterSeconds", retryAfter)
	logf.FromContext(ctx).Info("WARN: login rate limit exceeded; source address locked out",
		"remoteAddr", ip, "retryAfterSeconds", retryAfter)
}

func (a *Auth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func randomBase64URL(numBytes int) (string, error) {
	raw := make([]byte, numBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// memoryRevocationStore is the in-memory fallback when no SQLite store is passed.
type memoryRevocationStore struct {
	mu      sync.Mutex
	revoked map[string]int64
}

func newMemoryRevocationStore() *memoryRevocationStore {
	return &memoryRevocationStore{revoked: make(map[string]int64)}
}

func (m *memoryRevocationStore) RevokeSession(_ context.Context, jti string, now, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nowUnix := now.Unix()
	for k, exp := range m.revoked {
		if exp <= nowUnix {
			delete(m.revoked, k)
		}
	}
	m.revoked[jti] = expiresAt.Unix()
	return nil
}

func (m *memoryRevocationStore) IsSessionRevoked(_ context.Context, jti string, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.revoked[jti]
	if !ok {
		return false, nil
	}
	if exp <= now.Unix() {
		delete(m.revoked, jti)
		return false, nil
	}
	return true, nil
}

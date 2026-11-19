package dashboardauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/internal/history"
)

const (
	testBootstrapToken = "q7F2mZ0c1sV8pLbR4tYxN6hJ9kQwE3uA5dG7iO0aS1fD2gH4"
	testSigningKey     = "0123456789abcdef0123456789abcdef0123456789abcdef"
	rotatedSigningKey  = "fedcba9876543210fedcba9876543210fedcba9876543210"
)

var testStartTime = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	now time.Time
}

// testBaseURL is the browser-facing URL the tests configure.
const testBaseURL = "http://localhost:8080"

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newSQLiteRevocations(t *testing.T) *history.Store {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("open SQLite history store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestConfigValidatesModeAndSecrets(t *testing.T) {
	t.Parallel()
	validToken := func() (string, error) { return testBootstrapToken, nil }
	validKey := func() ([]byte, error) { return []byte(testSigningKey), nil }

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "rejects unknown mode",
			cfg:  Config{Mode: "oidc", AdminToken: validToken, SessionKey: validKey},
			want: "auth.mode",
		},
		{
			name: "token mode requires admin token source",
			cfg:  Config{Mode: ModeToken, SessionKey: validKey},
			want: "admin token",
		},
		{
			name: "token mode requires non-empty admin token at startup",
			cfg: Config{
				Mode:       ModeToken,
				AdminToken: func() (string, error) { return "", nil },
				SessionKey: validKey,
			},
			want: "no admin token",
		},
		{
			name: "token mode requires session key source",
			cfg:  Config{Mode: ModeToken, AdminToken: validToken},
			want: "session key",
		},
		{
			name: "token mode rejects short session key",
			cfg: Config{
				Mode:       ModeToken,
				AdminToken: validToken,
				SessionKey: func() ([]byte, error) { return []byte("too-short"), nil },
			},
			want: "32 bytes",
		},
		{
			name: "rejects invalid cookie secure setting",
			cfg: Config{
				Mode:         ModeToken,
				AdminToken:   validToken,
				SessionKey:   validKey,
				CookieSecure: "maybe",
			},
			want: "secure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New(%+v) error = %v, want substring %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestTokenLoginSetsSignedCookieAndSurvivesBootstrapSecretDeletion(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: testStartTime}
	var tokenDeleted atomic.Bool
	var delayed time.Duration

	auth, err := New(Config{
		Mode: ModeToken,
		AdminToken: func() (string, error) {
			if tokenDeleted.Load() {
				return "", errors.New("open /var/run/pagerless/admin/token: no such file or directory")
			}
			return testBootstrapToken, nil
		},
		SessionKey:  func() ([]byte, error) { return []byte(testSigningKey), nil },
		BaseURL:     "https://pagerless.example.com",
		SessionTTL:  8 * time.Hour,
		Revocations: newSQLiteRevocations(t),
		Now:         clock.Now,
		Sleep:       func(d time.Duration) { delayed += d },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	loginReq.RemoteAddr = "10.0.0.12:54321"
	loginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(loginRec, loginReq)

	if loginRec.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/login = %d: %s", loginRec.Code, loginRec.Body.String())
	}
	if delayed < DefaultLoginDelay {
		t.Errorf("login delay = %v, want at least %v", delayed, DefaultLoginDelay)
	}

	cookies := loginRec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want 1 session cookie", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != DefaultCookieName {
		t.Errorf("cookie.Name = %q, want %q", cookie.Name, DefaultCookieName)
	}
	if !cookie.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie.SameSite = %v, want SameSiteLaxMode", cookie.SameSite)
	}
	if !cookie.Secure {
		t.Error("cookie is not marked Secure for an HTTPS base URL")
	}
	if cookie.Path != "/" {
		t.Errorf("cookie.Path = %q, want /", cookie.Path)
	}

	// Verify the signed cookie wire format: b64url(payload) "." b64url(HMAC-SHA256).
	encodedPayload, encodedMAC, ok := strings.Cut(cookie.Value, ".")
	if !ok || encodedPayload == "" || encodedMAC == "" {
		t.Fatalf("cookie value %q is not payload.mac", cookie.Value)
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		V    int    `json:"v"`
		Sub  string `json:"sub"`
		JTI  string `json:"jti"`
		IAT  int64  `json:"iat"`
		EXP  int64  `json:"exp"`
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if claims.V != 1 || claims.Sub != "admin" || claims.JTI == "" || claims.CSRF == "" {
		t.Errorf("claims = %+v, want v=1 sub=admin and non-empty jti/csrf", claims)
	}
	if claims.IAT != testStartTime.Unix() || claims.EXP != testStartTime.Add(8*time.Hour).Unix() {
		t.Errorf("iat=%d exp=%d, want %d and %d",
			claims.IAT, claims.EXP, testStartTime.Unix(), testStartTime.Add(8*time.Hour).Unix())
	}

	// Post-login hardening step: deleting pagerless-admin-secret stops new token logins
	// while keeping the active signed-cookie session valid.
	tokenDeleted.Store(true)

	apiReq := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	apiReq.AddCookie(cookie)
	user := auth.User(apiReq)
	if user == nil || user.Username != "admin" || user.CSRF != claims.CSRF || !user.Approver {
		t.Fatalf("User after deleting bootstrap secret = %+v, want active admin session", user)
	}

	newLoginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	newLoginReq.RemoteAddr = "10.0.0.15:54321"
	newLoginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(newLoginRec, newLoginReq)
	if newLoginRec.Code != http.StatusUnauthorized {
		t.Errorf("login after deleting bootstrap secret = %d, want 401", newLoginRec.Code)
	}
}

func TestCookieSignatureIsVerifiedBeforeParsingPayloadAndRotationInvalidatesSessions(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: testStartTime}
	currentKey := []byte(testSigningKey)

	auth, err := New(Config{
		Mode:        ModeToken,
		AdminToken:  func() (string, error) { return testBootstrapToken, nil },
		SessionKey:  func() ([]byte, error) { return currentKey, nil },
		BaseURL:     testBaseURL,
		Revocations: newSQLiteRevocations(t),
		Now:         clock.Now,
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	loginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(loginRec, loginReq)
	cookie := loginRec.Result().Cookies()[0]
	if cookie.Secure {
		t.Error("cookie marked Secure for testBaseURL, want Secure=false")
	}

	// Flipping one character of the MAC must be rejected before payload parsing.
	tampered := *cookie
	last := tampered.Value[len(tampered.Value)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	tampered.Value = tampered.Value[:len(tampered.Value)-1] + string(replacement)

	req := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	req.AddCookie(&tampered)
	if got := auth.User(req); got != nil {
		t.Fatalf("tampered MAC accepted as %+v, want nil", got)
	}

	// A payload that is not even valid base64 or JSON with an invalid MAC is rejected cleanly by MAC check.
	unparseable := &http.Cookie{
		Name:  DefaultCookieName,
		Value: "%%%not-base64%%%." + strings.Split(cookie.Value, ".")[1],
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	req2.AddCookie(unparseable)
	if got := auth.User(req2); got != nil {
		t.Fatalf("unparseable payload with wrong MAC accepted as %+v, want nil", got)
	}

	// Rotating the session signing key signs every existing session out immediately.
	currentKey = []byte(rotatedSigningKey)
	validReq := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	validReq.AddCookie(cookie)
	if got := auth.User(validReq); got != nil {
		t.Fatalf("cookie signed with previous key accepted after rotation: %+v", got)
	}

	// Signing in again with the bootstrap token mints a session under the new key.
	reloginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	reloginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(reloginRec, reloginReq)
	newCookie := reloginRec.Result().Cookies()[0]
	reloginCheck := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	reloginCheck.AddCookie(newCookie)
	if got := auth.User(reloginCheck); got == nil {
		t.Fatal("cookie minted after key rotation was not accepted")
	}
}

func TestLogoutRequiresCSRFAndRevokesJTIInSQLite(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: testStartTime}
	store := newSQLiteRevocations(t)

	auth, err := New(Config{
		Mode:        ModeToken,
		AdminToken:  func() (string, error) { return testBootstrapToken, nil },
		SessionKey:  func() ([]byte, error) { return []byte(testSigningKey), nil },
		BaseURL:     testBaseURL,
		Revocations: store,
		Now:         clock.Now,
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	loginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(loginRec, loginReq)
	cookie := loginRec.Result().Cookies()[0]
	copiedCookie := *cookie

	checkReq := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	checkReq.AddCookie(cookie)
	user := auth.User(checkReq)
	if user == nil {
		t.Fatal("expected active session after login")
	}

	// Logout without CSRF token must fail with 403 and leave the session active.
	noCSRFReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	noCSRFReq.AddCookie(cookie)
	noCSRFRec := httptest.NewRecorder()
	auth.Logout().ServeHTTP(noCSRFRec, noCSRFReq)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403", noCSRFRec.Code)
	}
	if auth.User(checkReq) == nil {
		t.Fatal("failed logout without CSRF revoked the session")
	}

	// Logout with valid CSRF token revokes the JTI in SQLite and clears the cookie.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutReq.Header.Set("X-CSRF-Token", user.CSRF)
	logoutRec := httptest.NewRecorder()
	auth.Logout().ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout with CSRF = %d: %s, want 204", logoutRec.Code, logoutRec.Body.String())
	}

	// Reusing the copied cookie after logout must return nil (401).
	replayReq := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	replayReq.AddCookie(&copiedCookie)
	if got := auth.User(replayReq); got != nil {
		t.Fatalf("copied cookie still accepted after logout: %+v", got)
	}
	revoked, err := store.IsSessionRevoked(context.Background(), user.JTI, clock.Now())
	if err != nil || !revoked {
		t.Fatalf("IsSessionRevoked(%q) = %v, %v, want true", user.JTI, revoked, err)
	}
}

func TestLoginRateLimitLocksOutSourceAddressAndLogsWarning(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: testStartTime}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var delays int

	auth, err := New(Config{
		Mode:             ModeToken,
		AdminToken:       func() (string, error) { return testBootstrapToken, nil },
		SessionKey:       func() ([]byte, error) { return []byte(testSigningKey), nil },
		BaseURL:          "testBaseURL",
		LoginMaxAttempts: 5,
		LoginWindow:      5 * time.Minute,
		LoginLockout:     5 * time.Minute,
		Revocations:      newSQLiteRevocations(t),
		Now:              clock.Now,
		Sleep:            func(time.Duration) { delays++ },
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Five failed attempts from 192.0.2.10 within the window return 401.
	for i := 1; i <= 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"token":"wrong-token-attempt"}`))
		req.RemoteAddr = "192.0.2.10:40000"
		rec := httptest.NewRecorder()
		auth.Login().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}

	// The 6th attempt (even with the right token!) from 192.0.2.10 is answered with 429 + Retry-After.
	lockedReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	lockedReq.RemoteAddr = "192.0.2.10:40001"
	lockedRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(lockedRec, lockedReq)
	if lockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt = %d: %s, want 429", lockedRec.Code, lockedRec.Body.String())
	}
	if retryAfter := lockedRec.Header().Get("Retry-After"); retryAfter != "300" {
		t.Errorf("Retry-After = %q, want 300", retryAfter)
	}
	if delays != 6 {
		t.Errorf("fixed delay applied %d times, want 6", delays)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "192.0.2.10") {
		t.Errorf("warning log = %q, want level=WARN naming 192.0.2.10", logs)
	}
	if strings.Contains(logs, testBootstrapToken) || strings.Contains(logs, "wrong-token-attempt") {
		t.Errorf("log leaked a submitted token: %s", logs)
	}

	// Another IP is unaffected.
	otherReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	otherReq.RemoteAddr = "192.0.2.99:50000"
	otherRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusOK {
		t.Errorf("login from unaffected IP = %d, want 200", otherRec.Code)
	}

	// After the 5-minute lockout expires, 192.0.2.10 can sign in again.
	clock.Advance(5*time.Minute + time.Second)
	recoveredReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	recoveredReq.RemoteAddr = "192.0.2.10:40002"
	recoveredRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(recoveredRec, recoveredReq)
	if recoveredRec.Code != http.StatusOK {
		t.Errorf("login after lockout expiry = %d, want 200", recoveredRec.Code)
	}
}

func TestBearerTokenAndCSRFEnforcement(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: testStartTime}

	auth, err := New(Config{
		Mode:        ModeToken,
		AdminToken:  func() (string, error) { return testBootstrapToken, nil },
		SessionKey:  func() ([]byte, error) { return []byte(testSigningKey), nil },
		BaseURL:     testBaseURL,
		BearerToken: true,
		Revocations: newSQLiteRevocations(t),
		Now:         clock.Now,
		Sleep:       func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Valid bearer token authenticates and skips CSRF for that request alone.
	bearerReq := httptest.NewRequest(http.MethodPost, "/api/pages", nil)
	bearerReq.Header.Set("Authorization", "Bearer "+testBootstrapToken)
	bearerUser := auth.User(bearerReq)
	if bearerUser == nil || !bearerUser.Bearer || !bearerUser.Approver {
		t.Fatalf("User with valid bearer token = %+v, want bearer admin user", bearerUser)
	}
	if !auth.ValidateCSRF(bearerReq, bearerUser) {
		t.Error("ValidateCSRF = false for valid bearer token request, want true")
	}

	// Wrong bearer token is rejected even if a valid cookie is also attached.
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"token":"`+testBootstrapToken+`"}`))
	loginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(loginRec, loginReq)
	cookie := loginRec.Result().Cookies()[0]

	badBearerReq := httptest.NewRequest(http.MethodPost, "/api/pages", nil)
	badBearerReq.Header.Set("Authorization", "Bearer wrong-token")
	badBearerReq.AddCookie(cookie)
	if got := auth.User(badBearerReq); got != nil {
		t.Fatalf("wrong bearer token with cookie accepted as %+v, want nil", got)
	}

	// Cookie request requires matching X-CSRF-Token.
	cookieReq := httptest.NewRequest(http.MethodPost, "/api/pages", nil)
	cookieReq.AddCookie(cookie)
	cookieUser := auth.User(cookieReq)
	if cookieUser == nil || cookieUser.Bearer {
		t.Fatalf("cookieUser = %+v, want non-bearer user", cookieUser)
	}
	if auth.ValidateCSRF(cookieReq, cookieUser) {
		t.Error("ValidateCSRF = true without X-CSRF-Token header, want false")
	}
	cookieReq.Header.Set("X-CSRF-Token", "wrong-csrf")
	if auth.ValidateCSRF(cookieReq, cookieUser) {
		t.Error("ValidateCSRF = true with wrong X-CSRF-Token header, want false")
	}
	cookieReq.Header.Set("X-CSRF-Token", cookieUser.CSRF)
	if !auth.ValidateCSRF(cookieReq, cookieUser) {
		t.Error("ValidateCSRF = false with matching X-CSRF-Token header, want true")
	}

	// When BearerToken is disabled, Authorization: Bearer is refused.
	noBearerAuth, err := New(Config{
		Mode:        ModeToken,
		AdminToken:  func() (string, error) { return testBootstrapToken, nil },
		SessionKey:  func() ([]byte, error) { return []byte(testSigningKey), nil },
		BaseURL:     testBaseURL,
		BearerToken: false,
		Revocations: newSQLiteRevocations(t),
		Now:         clock.Now,
	})
	if err != nil {
		t.Fatalf("New(BearerToken=false): %v", err)
	}
	if got := noBearerAuth.User(bearerReq); got != nil {
		t.Fatalf("BearerToken=false accepted bearer token as %+v, want nil", got)
	}
}

func TestDisabledModeAndProxyIdentityHeader(t *testing.T) {
	t.Parallel()
	auth, err := New(Config{
		Mode:           ModeDisabled,
		IdentityHeader: "X-Auth-Request-User",
	})
	if err != nil {
		t.Fatalf("New(ModeDisabled): %v", err)
	}

	anonReq := httptest.NewRequest(http.MethodPost, "/api/pages", nil)
	anonUser := auth.User(anonReq)
	if anonUser == nil || anonUser.Username != "anonymous" || anonUser.Mode != ModeDisabled || !anonUser.Approver {
		t.Fatalf("anonymous user in disabled mode = %+v", anonUser)
	}
	if !auth.ValidateCSRF(anonReq, anonUser) {
		t.Error("ValidateCSRF = false in disabled mode, want true")
	}

	proxyReq := httptest.NewRequest(http.MethodPost, "/api/pages", nil)
	proxyReq.Header.Set("X-Auth-Request-User", "sre@example.com")
	proxyUser := auth.User(proxyReq)
	if proxyUser == nil || proxyUser.Username != "sre@example.com" || proxyUser.Subject != "sre@example.com" {
		t.Fatalf("proxied user in disabled mode = %+v, want sre@example.com", proxyUser)
	}

	// Login endpoint returns 403 in disabled mode.
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"anything"}`))
	loginRec := httptest.NewRecorder()
	auth.Login().ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusForbidden {
		t.Errorf("POST /api/auth/login in disabled mode = %d, want 403", loginRec.Code)
	}
}

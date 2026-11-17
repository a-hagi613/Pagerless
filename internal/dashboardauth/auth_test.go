package dashboardauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testClientID     = "pagerless-dashboard"
	testClientSecret = "test-secret"
	testUsername     = "alice"
	testRedirect     = "http://dashboard.test/auth/callback"
	testIssuer       = "http://issuer"
)

// fakeRealm is a Keycloak-shaped OpenID Connect provider. It signs ID tokens
// with its own key and verifies the PKCE challenge that the server sent.
type fakeRealm struct {
	server    *httptest.Server
	key       jose.JSONWebKey
	challenge string
	nonce     string
	groups    []string
	// tamper mutates the claims of the next ID token.
	tamper func(claims map[string]any)
}

func newFakeRealm(t *testing.T) *fakeRealm {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	realm := &fakeRealm{
		key:    jose.JSONWebKey{Key: key, KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig"},
		groups: []string{"pagerless-approvers"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                realm.server.URL,
			"authorization_endpoint":                realm.server.URL + "/authorize",
			"token_endpoint":                        realm.server.URL + "/token",
			"jwks_uri":                              realm.server.URL + "/keys",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"scopes_supported":                      []string{"openid", "profile", "email"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{realm.key.Public()}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("client_id") != testClientID || r.PostForm.Get("client_secret") != testClientSecret {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		if r.PostForm.Get("grant_type") != "authorization_code" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != realm.challenge {
			http.Error(w, "bad verifier", http.StatusBadRequest)
			return
		}
		claims := map[string]any{
			"iss":                realm.server.URL,
			"aud":                testClientID,
			"sub":                "subject-" + testUsername,
			"exp":                time.Now().Add(time.Hour).Unix(),
			"iat":                time.Now().Unix(),
			"preferred_username": testUsername,
			"email":              testUsername + "@example.com",
			"nonce":              realm.nonce,
			"groups":             realm.groups,
		}
		if realm.tamper != nil {
			realm.tamper(claims)
		}
		raw := realm.sign(t, claims)
		writeTestJSON(w, map[string]any{
			"access_token": "access", "token_type": "Bearer", "expires_in": 300, "id_token": raw,
		})
	})
	realm.server = httptest.NewServer(mux)
	t.Cleanup(realm.server.Close)
	return realm
}

// sign minted an RS256 ID token with the realm's key.
func (f *fakeRealm) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatalf("create a signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign the ID token: %v", err)
	}
	return raw
}

func writeTestJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// testAuth returns an Auth for the fake realm and a clock that a test can advance.
func testAuth(t *testing.T, realm *fakeRealm) (*Auth, *time.Time) {
	t.Helper()
	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	clock := &now
	auth, err := New(Config{
		Issuer:         realm.server.URL,
		ClientID:       testClientID,
		ClientSecret:   func() (string, error) { return testClientSecret, nil },
		RedirectURL:    testRedirect,
		ApproversGroup: "pagerless-approvers",
		SessionTTL:     2 * time.Hour,
		SecureCookies:  true,
		Now:            func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatalf("create the sign-in: %v", err)
	}
	return auth, clock
}

// begin starts the flow and returns the redirect URL and the flow cookie.
func begin(t *testing.T, auth *Auth) (*url.URL, *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	response := httptest.NewRecorder()
	auth.SignIn().ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("GET /login = %d, want 302", response.Code)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the redirect: %v", err)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == flowCookieName {
			return location, cookie
		}
	}
	t.Fatal("the sign-in response set no flow cookie")
	return nil, nil
}

// complete runs the callback for the flow cookie.
func complete(t *testing.T, auth *Auth, realm *fakeRealm, location *url.URL, flowCookie *http.Cookie) *http.Response {
	t.Helper()
	state := location.Query().Get("state")
	realm.challenge = location.Query().Get("code_challenge")
	realm.nonce = location.Query().Get("nonce")
	callback := "/auth/callback?code=test-code&state=" + url.QueryEscape(state)
	request := httptest.NewRequest(http.MethodGet, callback, nil)
	request.AddCookie(flowCookie)
	response := httptest.NewRecorder()
	auth.Callback().ServeHTTP(response, request)
	result := response.Result()
	t.Cleanup(func() { _ = result.Body.Close() })
	return result
}

func sessionCookie(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatal("the callback set no session cookie")
	return nil
}

func TestSignInCompletesWithPKCEAndRecordsTheKeycloakUser(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	auth, _ := testAuth(t, realm)

	location, flowCookie := begin(t, auth)
	query := location.Query()
	if got := query.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if query.Get("code_challenge") == "" || query.Get("nonce") == "" || query.Get("state") == "" {
		t.Errorf("the authorization request is missing PKCE, nonce, or state: %s", location)
	}
	if query.Get("client_id") != testClientID {
		t.Errorf("client_id = %q, want %q", query.Get("client_id"), testClientID)
	}
	if flowCookie.HttpOnly != true || flowCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("flow cookie = %+v, want HttpOnly and SameSite=Lax", flowCookie)
	}

	response := complete(t, auth, realm, location, flowCookie)
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		t.Fatalf("callback = %d %q, want 303 /", response.StatusCode, response.Header.Get("Location"))
	}
	cookie := sessionCookie(t, response)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Errorf("session cookie = %+v, want HttpOnly, Secure, SameSite=Lax, and Path=/", cookie)
	}
	if cookie.MaxAge != int((2 * time.Hour).Seconds()) {
		t.Errorf("session MaxAge = %d, want two hours", cookie.MaxAge)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	request.AddCookie(cookie)
	user := auth.User(request)
	if user == nil {
		t.Fatal("the session cookie resolves to no user")
	}
	if user.Username != testUsername || user.Subject != "subject-"+testUsername {
		t.Errorf("user = %+v, want the Keycloak preferred username and subject", user)
	}
	if !user.Approver {
		t.Error("a member of pagerless-approvers was not reported as an approver")
	}
	if user.CSRF == "" {
		t.Error("the session carries no CSRF token")
	}
}

func TestSignInRefusesATamperedStateOrNonce(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*fakeRealm, *http.Request)
	}{
		{
			name: "state",
			mutate: func(_ *fakeRealm, request *http.Request) {
				query := request.URL.Query()
				query.Set("state", "not-the-state")
				request.URL.RawQuery = query.Encode()
			},
		},
		{
			name: "nonce",
			mutate: func(realm *fakeRealm, _ *http.Request) {
				realm.tamper = func(claims map[string]any) { claims["nonce"] = "another-nonce" }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			realm := newFakeRealm(t)
			auth, _ := testAuth(t, realm)
			location, flowCookie := begin(t, auth)
			realm.challenge = location.Query().Get("code_challenge")
			realm.nonce = location.Query().Get("nonce")
			request := httptest.NewRequest(http.MethodGet,
				"/auth/callback?code=test-code&state="+url.QueryEscape(location.Query().Get("state")), nil)
			request.AddCookie(flowCookie)
			test.mutate(realm, request)
			response := httptest.NewRecorder()
			auth.Callback().ServeHTTP(response, request)
			if response.Code != http.StatusSeeOther ||
				!strings.HasPrefix(response.Header().Get("Location"), "/signed-out") {
				t.Fatalf("callback = %d %q, want a redirect to the signed-out page",
					response.Code, response.Header().Get("Location"))
			}
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == sessionCookieName && cookie.Value != "" {
					t.Fatal("a refused sign-in set a session cookie")
				}
			}
		})
	}
}

func TestSignInRefusesAMissingFlowCookie(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	auth, _ := testAuth(t, realm)
	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=y", nil)
	response := httptest.NewRecorder()
	auth.Callback().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/signed-out") {
		t.Fatalf("callback without a flow cookie = %d %q, want the signed-out page",
			response.Code, response.Header().Get("Location"))
	}
}

func TestSessionExpiresAndSignOutEndsIt(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	auth, now := testAuth(t, realm)
	location, flowCookie := begin(t, auth)
	response := complete(t, auth, realm, location, flowCookie)
	cookie := sessionCookie(t, response)

	signedIn := httptest.NewRequest(http.MethodGet, "/", nil)
	signedIn.AddCookie(cookie)
	user := auth.User(signedIn)
	if user == nil {
		t.Fatal("the session is not live")
	}

	signOut := httptest.NewRequest(http.MethodPost, "/logout", nil)
	signOut.AddCookie(cookie)
	signOut.Header.Set("X-CSRF-Token", user.CSRF)
	out := httptest.NewRecorder()
	auth.SignOut().ServeHTTP(out, signOut)
	if out.Code != http.StatusSeeOther || out.Header().Get("Location") != "/signed-out" {
		t.Fatalf("sign-out = %d %q, want 303 /signed-out", out.Code, out.Header().Get("Location"))
	}
	if auth.User(signedIn) != nil {
		t.Error("the session still resolves after sign-out")
	}

	location, flowCookie = begin(t, auth)
	cookie = sessionCookie(t, complete(t, auth, realm, location, flowCookie))
	*now = now.Add(3 * time.Hour)
	expired := httptest.NewRequest(http.MethodGet, "/", nil)
	expired.AddCookie(cookie)
	if auth.User(expired) != nil {
		t.Error("an expired session still resolves")
	}
}

func TestSignOutRequiresTheCSRFToken(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	auth, _ := testAuth(t, realm)
	location, flowCookie := begin(t, auth)
	cookie := sessionCookie(t, complete(t, auth, realm, location, flowCookie))

	request := httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	auth.SignOut().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("sign-out without a CSRF token = %d, want 403", response.Code)
	}
	still := httptest.NewRequest(http.MethodGet, "/", nil)
	still.AddCookie(cookie)
	if auth.User(still) == nil {
		t.Error("a refused sign-out ended the session")
	}
}

func TestOnlyGroupMembersMayApprove(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	realm.groups = []string{"another-group"}
	auth, _ := testAuth(t, realm)
	location, flowCookie := begin(t, auth)
	cookie := sessionCookie(t, complete(t, auth, realm, location, flowCookie))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	user := auth.User(request)
	if user == nil {
		t.Fatal("the session is not live")
	}
	if user.Approver {
		t.Errorf("user groups = %v, want no approver status", user.Groups)
	}
}

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	secret := func() (string, error) { return testClientSecret, nil }
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{name: "issuer", cfg: Config{ClientID: testClientID, ClientSecret: secret, RedirectURL: testRedirect}},
		{name: "client", cfg: Config{Issuer: testIssuer, ClientSecret: secret, RedirectURL: testRedirect}},
		{name: "secret", cfg: Config{Issuer: testIssuer, ClientID: testClientID, RedirectURL: testRedirect}},
		{name: "redirect", cfg: Config{Issuer: testIssuer, ClientID: testClientID, ClientSecret: secret}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.cfg); err == nil {
				t.Errorf("New accepted a configuration without %s", test.name)
			}
		})
	}
}

// TestIssuerIsDiscoveredLazily makes sure a server starts before Keycloak does.
func TestIssuerIsDiscoveredLazily(t *testing.T) {
	t.Parallel()
	auth, err := New(Config{
		Issuer:       "http://127.0.0.1:1/realms/pagerless",
		ClientID:     testClientID,
		ClientSecret: func() (string, error) { return testClientSecret, nil },
		RedirectURL:  "http://dashboard.test/auth/callback",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if auth.User(httptest.NewRequest(http.MethodGet, "/", nil)) != nil {
		t.Error("an unauthenticated request resolved to a user")
	}
	response := httptest.NewRecorder()
	auth.SignIn().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/login", nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("GET /login with an unreachable issuer = %d, want a redirect to the signed-out page", response.Code)
	}
}

func TestCSRFTokenIsUniquePerSession(t *testing.T) {
	t.Parallel()
	realm := newFakeRealm(t)
	auth, _ := testAuth(t, realm)
	first, firstFlow := begin(t, auth)
	firstCookie := sessionCookie(t, complete(t, auth, realm, first, firstFlow))
	second, secondFlow := begin(t, auth)
	secondCookie := sessionCookie(t, complete(t, auth, realm, second, secondFlow))

	one := httptest.NewRequest(http.MethodGet, "/", nil)
	one.AddCookie(firstCookie)
	two := httptest.NewRequest(http.MethodGet, "/", nil)
	two.AddCookie(secondCookie)
	if auth.User(one).CSRF == auth.User(two).CSRF {
		t.Error("two sessions share a CSRF token")
	}
}

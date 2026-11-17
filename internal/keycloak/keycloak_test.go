package keycloak

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRealm  = "pagerless"
	testClient = "pagerless-server"
	testSecret = "test-secret"
	testBase   = "http://keycloak"
)

// fakeKeycloak serves the admin API and the client-credentials token endpoint.
type fakeKeycloak struct {
	server     *httptest.Server
	mu         sync.Mutex
	tokens     int
	lookups    int
	failAdmin  bool
	expiresIn  int
	lastBearer string
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	fake := &fakeKeycloak{expiresIn: 300}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/"+testRealm+"/protocol/openid-connect/token",
		func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if r.PostForm.Get("grant_type") != "client_credentials" ||
				r.PostForm.Get("client_id") != testClient || r.PostForm.Get("client_secret") != testSecret {
				http.Error(w, "bad client", http.StatusUnauthorized)
				return
			}
			fake.mu.Lock()
			fake.tokens++
			expires := fake.expiresIn
			fake.mu.Unlock()
			writeJSON(w, map[string]any{"access_token": "admin-token", "expires_in": expires})
		})
	mux.HandleFunc("GET /admin/realms/"+testRealm+"/users", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.lookups++
		fail := fake.failAdmin
		fake.lastBearer = r.Header.Get("Authorization")
		fake.mu.Unlock()
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("exact") != "true" || r.URL.Query().Get("username") != "alice" {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{map[string]any{"id": "user-1", "username": "alice"}})
	})
	mux.HandleFunc("GET /admin/realms/"+testRealm+"/users/user-1/groups", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fail := fake.failAdmin
		fake.mu.Unlock()
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, []any{
			map[string]any{"name": "pagerless-approvers"},
			map[string]any{"name": "another-group"},
		})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeKeycloak) membership(t *testing.T) *Membership {
	t.Helper()
	clock := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	m, err := New(Config{
		BaseURL:      f.server.URL,
		Realm:        testRealm,
		ClientID:     testClient,
		ClientSecret: func() (string, error) { return testSecret, nil },
		Now:          func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeKeycloak) counts() (tokens, lookups int, bearer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens, f.lookups, f.lastBearer
}

func TestIsApproverChecksTheGroupAndCachesTheAnswer(t *testing.T) {
	t.Parallel()
	fake := newFakeKeycloak(t)
	membership := fake.membership(t)

	approver, err := membership.IsApprover(t.Context(), "alice")
	if err != nil {
		t.Fatalf("IsApprover: %v", err)
	}
	if !approver {
		t.Error("alice is a member of pagerless-approvers but was not reported as an approver")
	}
	if _, err := membership.IsApprover(t.Context(), "alice"); err != nil {
		t.Fatalf("IsApprover again: %v", err)
	}
	tokens, lookups, bearer := fake.counts()
	if tokens != 1 {
		t.Errorf("token requests = %d, want one cached service-account token", tokens)
	}
	if lookups != 1 {
		t.Errorf("user lookups = %d, want one cached membership answer", lookups)
	}
	if bearer != "Bearer admin-token" {
		t.Errorf("Authorization = %q, want the service-account token", bearer)
	}
}

func TestIsApproverRefusesAStranger(t *testing.T) {
	t.Parallel()
	fake := newFakeKeycloak(t)
	membership := fake.membership(t)

	approver, err := membership.IsApprover(t.Context(), "mallory")
	if err != nil {
		t.Fatalf("IsApprover: %v", err)
	}
	if approver {
		t.Error("a user that Keycloak does not know was reported as an approver")
	}
	if approver, err := membership.IsApprover(t.Context(), ""); err != nil || approver {
		t.Errorf("IsApprover(empty) = %v, %v, want false", approver, err)
	}
}

func TestIsApproverFailsClosedWhenKeycloakIsUnavailable(t *testing.T) {
	t.Parallel()
	fake := newFakeKeycloak(t)
	fake.failAdmin = true
	membership := fake.membership(t)

	if _, err := membership.IsApprover(t.Context(), "alice"); err == nil {
		t.Fatal("IsApprover succeeded while Keycloak was unavailable, want an error")
	}
}

func TestAdminTokenIsRenewedBeforeItExpires(t *testing.T) {
	t.Parallel()
	fake := newFakeKeycloak(t)
	fake.expiresIn = 10 // shorter than the safety margin, so the token is renewed at its expiry.
	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	membership, err := New(Config{
		BaseURL:      fake.server.URL,
		Realm:        testRealm,
		ClientID:     testClient,
		ClientSecret: func() (string, error) { return testSecret, nil },
		Now:          func() time.Time { return now },
		CacheTTL:     time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := membership.IsApprover(t.Context(), "alice"); err != nil {
		t.Fatalf("IsApprover: %v", err)
	}
	now = now.Add(11 * time.Second)
	if _, err := membership.IsApprover(t.Context(), "alice"); err != nil {
		t.Fatalf("IsApprover: %v", err)
	}
	tokens, _, _ := fake.counts()
	if tokens != 2 {
		t.Errorf("token requests = %d, want a renewal after the token expired", tokens)
	}
}

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	secret := func() (string, error) { return testSecret, nil }
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{name: "base URL", cfg: Config{Realm: testRealm, ClientID: testClient, ClientSecret: secret}},
		{name: "realm", cfg: Config{BaseURL: testBase, ClientID: testClient, ClientSecret: secret}},
		{name: "client", cfg: Config{BaseURL: testBase, Realm: testRealm, ClientSecret: secret}},
		{name: "secret", cfg: Config{BaseURL: testBase, Realm: testRealm, ClientID: testClient}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.cfg); err == nil || !strings.Contains(err.Error(), "required") {
				t.Errorf("New = %v, want a refusal naming the missing %s", err, test.name)
			}
		})
	}
}

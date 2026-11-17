package testenv

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	// ArgoCDApplication is the application that the fake Argo CD serves, as in a cluster.
	ArgoCDApplication = "sandbox"
	// ArgoCDToken is the only bearer token that the fake Argo CD accepts.
	ArgoCDToken = "argocd_fake_api_token"
	// errorKey and statusKey are the JSON keys that the fakes repeat across their responses.
	errorKey  = "error"
	statusKey = "status"
)

// ArgoCDStatus is the status of an application on the fake Argo CD.
type ArgoCDStatus struct {
	// Sync is Synced, OutOfSync, or Unknown.
	Sync string
	// Revision is the commit the application is synced to.
	Revision string
	// Health is Healthy, Progressing, Degraded, and so on.
	Health string
}

// FakeArgoCD stands in for the part of the Argo CD API that Verification uses: reading an application. Its
// application starts OutOfSync and Progressing, as one that has not yet picked up a merged commit. A test moves
// it with Set or Sync.
type FakeArgoCD struct {
	// URL is the base URL of the API.
	URL string

	mu       sync.Mutex
	apps     map[string]ArgoCDStatus
	requests []string
	failures int
}

// NewFakeArgoCD starts a fake Argo CD.
func NewFakeArgoCD(t testing.TB) *FakeArgoCD {
	t.Helper()
	a := &FakeArgoCD{apps: map[string]ArgoCDStatus{
		ArgoCDApplication: {Sync: "OutOfSync", Health: "Progressing"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	a.URL = srv.URL
	return a
}

// Token returns the token that pagerless-server must present, so the harness can configure it.
func (a *FakeArgoCD) Token() string { return ArgoCDToken }

// Set replaces the status of the application.
func (a *FakeArgoCD) Set(name string, status ArgoCDStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.apps[name] = status
}

// Sync reports the Sandbox's application Synced and Healthy at the revision.
func (a *FakeArgoCD) Sync(revision string) {
	a.Set(ArgoCDApplication, ArgoCDStatus{Sync: "Synced", Revision: revision, Health: "Healthy"})
}

// FailNext makes the next count requests answer 502.
func (a *FakeArgoCD) FailNext(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = count
}

// Requests returns every request received, as "METHOD /path", in order.
func (a *FakeArgoCD) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

func (a *FakeArgoCD) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+ArgoCDToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{errorKey: "invalid session", "code": 16})
		return
	}
	if a.failures > 0 {
		a.failures--
		writeJSON(w, http.StatusBadGateway, map[string]any{errorKey: "injected failure"})
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/api/v1/applications/")
	status, found := a.apps[name]
	if r.Method != http.MethodGet || !ok || !found {
		writeJSON(w, http.StatusNotFound, map[string]any{errorKey: fmt.Sprintf("application %q not found", name), "code": 5})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "argocd"},
		statusKey: map[string]any{
			"sync":   map[string]any{statusKey: status.Sync, "revision": status.Revision},
			"health": map[string]any{statusKey: status.Health},
			// Argo CD lists every managed resource, which Verification must tolerate.
			"resources": []map[string]any{{"kind": "Deployment", "name": "checkout", statusKey: status.Sync}},
		},
	})
}

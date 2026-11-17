package testenv

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// FakeAlertmanager stands in for the part of the Alertmanager v2 API that Verification uses: listing alerts.
// Like the real one, it lists an alert until it resolves.
type FakeAlertmanager struct {
	// URL is the base URL of the API.
	URL string

	mu       sync.Mutex
	alerts   map[string]fakeAlert
	requests []string
	failures int
}

type fakeAlert struct {
	state  string
	endsAt time.Time
}

// NewFakeAlertmanager starts a fake Alertmanager with no alerts.
func NewFakeAlertmanager(t testing.TB) *FakeAlertmanager {
	t.Helper()
	a := &FakeAlertmanager{alerts: map[string]fakeAlert{}}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	a.URL = srv.URL
	return a
}

// Fire makes the alert with the fingerprint active.
func (a *FakeAlertmanager) Fire(fingerprint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts[fingerprint] = fakeAlert{state: "active"}
}

// Silence makes the alert suppressed. It still fires; only its notifications are held back.
func (a *FakeAlertmanager) Silence(fingerprint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts[fingerprint] = fakeAlert{state: "suppressed"}
}

// Resolve removes the alert, as Alertmanager does once an alert resolves.
func (a *FakeAlertmanager) Resolve(fingerprint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.alerts, fingerprint)
}

// EndAt keeps the alert listed, but with an end time. An alert that ends in the past is resolved.
func (a *FakeAlertmanager) EndAt(fingerprint string, endsAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts[fingerprint] = fakeAlert{state: "active", endsAt: endsAt}
}

// FailNext makes the next count requests answer 503.
func (a *FakeAlertmanager) FailNext(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = count
}

// Requests returns every request received, as "METHOD /path", in order.
func (a *FakeAlertmanager) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

func (a *FakeAlertmanager) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	if a.failures > 0 {
		a.failures--
		writeJSON(w, http.StatusServiceUnavailable, message("injected failure"))
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/api/v2/alerts" {
		writeJSON(w, http.StatusNotFound, message("not found"))
		return
	}
	listed := []map[string]any{}
	for fingerprint, alert := range a.alerts {
		entry := map[string]any{
			"fingerprint": fingerprint,
			"labels":      map[string]string{"alertname": "KubePodOOMKilled"},
			statusKey:     map[string]any{"state": alert.state, "silencedBy": []string{}, "inhibitedBy": []string{}},
		}
		if !alert.endsAt.IsZero() {
			entry["endsAt"] = alert.endsAt.UTC().Format(time.RFC3339)
		} else {
			// Alertmanager sends the zero time for an alert without an end.
			entry["endsAt"] = "0001-01-01T00:00:00.000Z"
		}
		listed = append(listed, entry)
	}
	writeJSON(w, http.StatusOK, listed)
}

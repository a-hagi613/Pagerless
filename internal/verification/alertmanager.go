package verification

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/utils/clock"
)

// Alertmanager reads alerts from the Alertmanager v2 API.
type Alertmanager struct {
	// BaseURL is the base URL of Alertmanager.
	BaseURL string
	// HTTP is the HTTP client. It defaults to a client with a request timeout.
	HTTP *http.Client
	// Clock decides whether a listed alert's end time has passed. It defaults to the real clock.
	Clock clock.PassiveClock
}

// Firing reports whether the alert with the fingerprint has not resolved. An alert resolves when Alertmanager no
// longer lists it, or lists it with an end time in the past. A silenced or inhibited alert has not resolved: it
// still fires, and only its notifications are held back.
func (c *Alertmanager) Firing(ctx context.Context, fingerprint string) (bool, error) {
	query := url.Values{}
	for _, state := range []string{"active", "silenced", "inhibited", "unprocessed"} {
		query.Set(state, "true")
	}
	endpoint := strings.TrimSuffix(c.BaseURL, "/") + "/api/v2/alerts?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("build the Alertmanager request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	var alerts []struct {
		Fingerprint string    `json:"fingerprint"`
		EndsAt      time.Time `json:"endsAt"`
	}
	if err := doJSON(httpClient(c.HTTP), req, "Alertmanager", &alerts); err != nil {
		return false, fmt.Errorf("list Alertmanager alerts: %w", err)
	}
	var clk clock.PassiveClock = clock.RealClock{}
	if c.Clock != nil {
		clk = c.Clock
	}
	now := clk.Now()
	for _, a := range alerts {
		if a.Fingerprint == fingerprint && (a.EndsAt.IsZero() || a.EndsAt.After(now)) {
			return true, nil
		}
	}
	return false, nil
}

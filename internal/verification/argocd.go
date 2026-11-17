// Package verification holds the deterministic checks that decide whether a Workload recovered after its
// remediation was executed (ADR-0006), and the clients of the Argo CD and Alertmanager APIs that they read.
package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxResponseBytes bounds a response from Argo CD or Alertmanager. An Argo CD application lists every
	// resource it manages, so the bound is generous.
	maxResponseBytes = 16 << 20
	// requestTimeout bounds a request when the client has no HTTP client of its own.
	requestTimeout = 30 * time.Second
	// maxErrorBytes bounds the part of an error response that an error quotes.
	maxErrorBytes = 200
)

// Application is the part of an Argo CD application's status that Verification reads.
type Application struct {
	// SyncStatus is Argo CD's sync status: Synced, OutOfSync, or Unknown.
	SyncStatus string
	// Revision is the commit that the application is synced to.
	Revision string
	// HealthStatus is Argo CD's health status: Healthy, Progressing, Degraded, and so on.
	HealthStatus string
}

// ArgoCD reads applications from the Argo CD API. It needs only the permission to get an application.
type ArgoCD struct {
	// BaseURL is the base URL of the Argo CD API server.
	BaseURL string
	// Token returns the bearer token that authenticates to Argo CD. It is called for every request, so a rotated
	// token is picked up.
	Token func() (string, error)
	// HTTP is the HTTP client. It defaults to a client with a request timeout.
	HTTP *http.Client
}

// Application returns the named application's status.
func (c *ArgoCD) Application(ctx context.Context, name string) (*Application, error) {
	token, err := c.Token()
	if err != nil {
		return nil, fmt.Errorf("read the Argo CD token: %w", err)
	}
	endpoint := strings.TrimSuffix(c.BaseURL, "/") + "/api/v1/applications/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build the Argo CD request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	var body struct {
		Status struct {
			Sync struct {
				Status   string `json:"status"`
				Revision string `json:"revision"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
		} `json:"status"`
	}
	if err := doJSON(httpClient(c.HTTP), req, "Argo CD", &body); err != nil {
		return nil, fmt.Errorf("get Argo CD application %s: %w", name, err)
	}
	return &Application{
		SyncStatus:   body.Status.Sync.Status,
		Revision:     body.Status.Sync.Revision,
		HealthStatus: body.Status.Health.Status,
	}, nil
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: requestTimeout}
}

// doJSON sends the request and decodes a 200 response into out.
func doJSON(c *http.Client, req *http.Request, service string, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", service, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		quoted, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return fmt.Errorf("%s answered %s: %s", service, resp.Status, strings.TrimSpace(string(quoted)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decode the %s response: %w", service, err)
	}
	return nil
}

// Package alertstate holds the read-only Connectors that answer "which alerts
// are firing for this Target". Alert state is a third capability: it is neither
// a log stream nor a metric series.
package alertstate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Alertmanager reads an Alertmanager's firing alerts.
type Alertmanager struct {
	client            *base.Client
	filterByNamespace bool
}

// NewAlertmanager builds the Alertmanager alert-state Connector.
func NewAlertmanager(client *base.Client, filterByNamespace bool) *Alertmanager {
	return &Alertmanager{client: client, filterByNamespace: filterByNamespace}
}

// Name returns the connector name.
func (a *Alertmanager) Name() string { return "alertmanager" }

// QueryAlerts reads /api/v2/alerts and keeps the alerts that name the Target.
func (a *Alertmanager) QueryAlerts(ctx context.Context, q contract.AlertQuery) ([]contract.AlertState, error) {
	values := url.Values{}
	values.Set("active", "true")
	values.Set("silenced", "false")
	values.Set("inhibited", "false")
	values.Set("unprocessed", "false")
	if a.filterByNamespace {
		values.Set("filter", fmt.Sprintf("namespace=%q", q.Namespace))
	}
	response, err := a.client.Get(ctx, "/api/v2/alerts", values)
	if err != nil {
		return nil, err
	}
	return decodeAlerts(response.Body, q, a.filterByNamespace)
}

// Grafana reads the firing alerts of Grafana's embedded Alertmanager.
type Grafana struct {
	client *base.Client
}

// NewGrafana builds the Grafana alert-state Connector.
func NewGrafana(client *base.Client) *Grafana {
	return &Grafana{client: client}
}

// Name returns the connector name.
func (g *Grafana) Name() string { return "grafana" }

// QueryAlerts reads Grafana's Alertmanager-compatible endpoint.
func (g *Grafana) QueryAlerts(ctx context.Context, q contract.AlertQuery) ([]contract.AlertState, error) {
	values := url.Values{}
	values.Set("active", "true")
	values.Set("silenced", "false")
	values.Set("inhibited", "false")
	response, err := g.client.Get(ctx, "/api/alertmanager/grafana/api/v2/alerts", values)
	if err != nil {
		return nil, err
	}
	return decodeAlerts(response.Body, q, true)
}

// alertResponse is one Alertmanager alert.
type alertResponse struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
	Fingerprint string            `json:"fingerprint"`
	Status      struct {
		State string `json:"state"`
	} `json:"status"`
}

// decodeAlerts parses the Alertmanager response and keeps the matching alerts.
func decodeAlerts(raw []byte, q contract.AlertQuery, matchNamespaceOnly bool) ([]contract.AlertState, error) {
	var decoded []alertResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode the Alertmanager response: %w", err)
	}
	alerts := make([]contract.AlertState, 0, len(decoded))
	for _, alert := range decoded {
		if alert.Labels["namespace"] != q.Namespace {
			continue
		}
		if !matchNamespaceOnly && !namesTarget(alert.Labels, q.Subject.Name) {
			continue
		}
		state := contract.AlertState{
			Name: alert.Labels["alertname"], State: alert.Status.State, Fingerprint: alert.Fingerprint,
			Labels: alert.Labels, StartsAt: parseTime(alert.StartsAt),
		}
		if ends := parseTime(alert.EndsAt); !ends.IsZero() {
			state.EndsAt = &ends
		}
		alerts = append(alerts, state)
	}
	return alerts, nil
}

// namesTarget reports whether the labels name the Target.
func namesTarget(labels map[string]string, target string) bool {
	if labels["deployment"] == target || labels["statefulset"] == target || labels["daemonset"] == target ||
		labels["job"] == target || labels["cronjob"] == target || labels["pod"] == target {
		return true
	}
	return labels["pagerless.dev/workload-name"] == target
}

// parseTime parses an Alertmanager timestamp, tolerating the zero value.
func parseTime(raw string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// assert the interfaces.
var (
	_ contract.AlertStateConnector = (*Alertmanager)(nil)
	_ contract.AlertStateConnector = (*Grafana)(nil)
)

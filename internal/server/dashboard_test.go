package server_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/metrics"
)

// The product's own Grafana dashboard ships inside the chart; the target
// dashboards and the ServiceMonitor moved to the showcase repository with the
// rest of the demonstration environment.
const dashboardFile = "../../charts/pagerless/dashboards/pagerless.json"

func TestGrafanaDashboardChartsEveryExportedMetric(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		UID    string            `json:"uid"`
		Title  string            `json:"title"`
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("the dashboard is not valid JSON: %v", err)
	}
	if dashboard.UID != "pagerless" || dashboard.Title != "Pagerless" {
		t.Errorf("dashboard = %q (%s), want the Pagerless dashboard", dashboard.Title, dashboard.UID)
	}
	if len(dashboard.Panels) < 6 {
		t.Errorf("dashboard has %d panels, want one for each exported metric", len(dashboard.Panels))
	}
	for _, name := range []string{
		metrics.MetricPages,
		metrics.MetricProposals,
		metrics.MetricDecisions,
		metrics.MetricVerifications,
		metrics.MetricTimeToRecovered,
		metrics.MetricInputTokens,
		metrics.MetricOutputTokens,
		metrics.MetricCostUSD,
	} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("the dashboard does not chart %s", name)
		}
	}
}

package dashboardapi

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// DefaultMetricsDashboardUID is the Grafana dashboard that charts a Workload's metrics. It is the default for
	// --grafana-workload-dashboard-uid and matches the dashboard provisioned in deploy/observability/dashboards.
	DefaultMetricsDashboardUID = "pagerless-workload"
	// DefaultLogsDashboardUID is the Grafana dashboard that shows a Workload's logs. It is the default for
	// --grafana-logs-dashboard-uid and matches the dashboard provisioned in deploy/observability/dashboards.
	DefaultLogsDashboardUID = "pagerless-workload-logs"
	// metricsSlug and logsSlug are the URL slugs that Grafana renders from the dashboard titles.
	metricsSlug = "workload-metrics"
	logsSlug    = "workload-logs"
	// windowLead and windowTail widen the Workload's lifetime, so that the panels show the minutes before the
	// Triage started and after it ended.
	windowLead = 15 * time.Minute
	windowTail = 5 * time.Minute
)

// GrafanaConfig configures the embedded Grafana panels. An empty BaseURL disables them.
type GrafanaConfig struct {
	// BaseURL is the Grafana base URL that the browser can reach, for example http://localhost:3000 while the
	// Grafana Service is port-forwarded.
	BaseURL string
	// MetricsDashboardUID is the Grafana dashboard that charts the Workload's metrics.
	MetricsDashboardUID string
	// LogsDashboardUID is the Grafana dashboard that shows the Workload's logs.
	LogsDashboardUID string
}

// GrafanaLinks are the embedded panels and deep links for one Triage's Workload.
type GrafanaLinks struct {
	BaseURL string         `json:"baseURL"`
	Metrics *DashboardLink `json:"metrics,omitempty"`
	Logs    *DashboardLink `json:"logs,omitempty"`
}

// DashboardLink is one Grafana dashboard, embedded and opened directly.
type DashboardLink struct {
	EmbedURL string `json:"embedURL"`
	DeepLink string `json:"deepLink"`
}

// links builds the Grafana links for a Workload over the Triage's lifetime, or nil when Grafana is unconfigured.
func (g GrafanaConfig) links(
	spec *v1alpha1.TriageSpec, status *v1alpha1.TriageStatus, createdAt, now time.Time,
) *GrafanaLinks {
	base := strings.TrimRight(strings.TrimSpace(g.BaseURL), "/")
	if base == "" {
		return nil
	}
	from := createdAt
	if status.StartedAt != nil {
		from = status.StartedAt.Time
	}
	if from.IsZero() {
		from = now.Add(-time.Hour)
	}
	to := now
	if status.TerminalAt != nil {
		to = status.TerminalAt.Time
	}
	if !to.After(from) {
		to = from.Add(windowTail)
	}
	from = from.Add(-windowLead)
	to = to.Add(windowTail)

	query := url.Values{}
	query.Set("var-namespace", spec.Workload.Namespace)
	query.Set("var-workload", spec.Workload.Name)
	query.Set("from", strconv.FormatInt(from.UnixMilli(), 10))
	query.Set("to", strconv.FormatInt(to.UnixMilli(), 10))

	links := &GrafanaLinks{BaseURL: base}
	if g.MetricsDashboardUID != "" {
		links.Metrics = dashboardLink(base, g.MetricsDashboardUID, metricsSlug, query)
	}
	if g.LogsDashboardUID != "" {
		links.Logs = dashboardLink(base, g.LogsDashboardUID, logsSlug, query)
	}
	return links
}

// dashboardLink returns the embed URL, which Grafana renders without its navigation, and the deep link, which
// opens the same dashboard and time range in Grafana as a fallback when embedding is unavailable.
func dashboardLink(base, uid, slug string, query url.Values) *DashboardLink {
	escaped := url.PathEscape(uid)
	embed := url.Values{}
	for name, values := range query {
		embed[name] = append([]string(nil), values...)
	}
	embed.Set("kiosk", "")
	return &DashboardLink{
		EmbedURL: base + "/d-solo/" + escaped + "/" + slug + "?" + embed.Encode(),
		DeepLink: base + "/d/" + escaped + "/" + slug + "?" + query.Encode(),
	}
}

package dashboardapi

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	metricsKey = "metrics"
	logsKey    = "logs"
)

func TestGrafanaLinksEmbedAndDeepLinkTheWorkload(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	terminal := started.Add(5 * time.Minute)
	now := started.Add(time.Hour)
	spec := &v1alpha1.TriageSpec{Workload: v1alpha1.WorkloadReference{
		Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "checkout",
	}}
	status := &v1alpha1.TriageStatus{
		StartedAt:  &metav1.Time{Time: started},
		TerminalAt: &metav1.Time{Time: terminal},
	}

	links := GrafanaConfig{
		BaseURL:             "http://localhost:3000/",
		MetricsDashboardUID: DefaultMetricsDashboardUID,
		LogsDashboardUID:    DefaultLogsDashboardUID,
	}.links(spec, status, started, now)
	if links == nil {
		t.Fatal("links are nil with a configured Grafana base URL")
	}
	if links.BaseURL != "http://localhost:3000" {
		t.Errorf("base URL = %q, want the trailing slash trimmed", links.BaseURL)
	}
	if links.Metrics == nil || links.Logs == nil {
		t.Fatalf("links = %+v, want both dashboards", links)
	}

	for name, link := range map[string]*DashboardLink{metricsKey: links.Metrics, logsKey: links.Logs} {
		embed, err := url.Parse(link.EmbedURL)
		if err != nil {
			t.Fatalf("%s embed URL does not parse: %v", name, err)
		}
		deep, err := url.Parse(link.DeepLink)
		if err != nil {
			t.Fatalf("%s deep link does not parse: %v", name, err)
		}
		if embed.Path != "/d-solo/"+map[string]string{
			metricsKey: DefaultMetricsDashboardUID, logsKey: DefaultLogsDashboardUID,
		}[name]+"/"+map[string]string{metricsKey: metricsSlug, logsKey: logsSlug}[name] {
			t.Errorf("%s embed path = %q", name, embed.Path)
		}
		if deep.Path == embed.Path {
			t.Errorf("%s deep link path = %q, want the full dashboard", name, deep.Path)
		}
		query := embed.Query()
		for key, want := range map[string]string{
			"var-namespace": "sandbox",
			"var-workload":  "checkout",
			"from":          strconv.FormatInt(started.Add(-windowLead).UnixMilli(), 10),
			"to":            strconv.FormatInt(terminal.Add(windowTail).UnixMilli(), 10),
		} {
			if got := query.Get(key); got != want {
				t.Errorf("%s embed %s = %q, want %q", name, key, got, want)
			}
		}
		if _, kiosk := query["kiosk"]; !kiosk {
			t.Errorf("%s embed URL lacks kiosk, so Grafana would show its navigation", name)
		}
		if _, kiosk := deep.Query()["kiosk"]; kiosk {
			t.Errorf("%s deep link carries kiosk, which belongs only to the embed", name)
		}
	}
}

func TestGrafanaLinksAreAbsentWithoutABaseURL(t *testing.T) {
	t.Parallel()
	links := GrafanaConfig{MetricsDashboardUID: "a", LogsDashboardUID: "b"}.
		links(&v1alpha1.TriageSpec{}, &v1alpha1.TriageStatus{}, time.Now(), time.Now())
	if links != nil {
		t.Errorf("links = %+v, want none when Grafana is not configured", links)
	}
}

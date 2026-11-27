package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusFixtureServesTheSandboxAlerts(t *testing.T) {
	server := httptest.NewServer(prometheusHandler())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/alerts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	for _, alert := range []string{"ContainerOOMKilled", "ServiceErrorRateHigh", "ContainerMemoryGrowth"} {
		if !strings.Contains(string(body), alert) {
			t.Fatalf("the fixture does not contain %s", alert)
		}
	}
}

func TestGrafanaFixtureServesDashboardsAndDataSources(t *testing.T) {
	server := httptest.NewServer(grafanaHandler())
	defer server.Close()
	for path, want := range map[string]string{
		"/api/search":                             "pagerless-target",
		"/api/dashboards/uid/pagerless-target":    "Pagerless Target metrics",
		"/api/alertmanager/grafana/api/v2/alerts": "ContainerOOMKilled",
	} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s does not contain %q", path, want)
		}
	}
}

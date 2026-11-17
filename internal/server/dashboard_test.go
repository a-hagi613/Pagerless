package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/internal/metrics"
	"github.com/a-hagi613/pagerless/internal/server"
)

const (
	observabilityDir = "../../deploy/observability"
	// pagerlessName is both the server's namespace and the dashboard's UID.
	pagerlessName = "pagerless"
)

func TestGrafanaDashboardIsProvisionedForPagerlessMetrics(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(observabilityDir, "dashboards", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		GeneratorOptions struct {
			Labels map[string]string `json:"labels"`
		} `json:"generatorOptions"`
		ConfigMapGenerator []struct {
			Name  string   `json:"name"`
			Files []string `json:"files"`
		} `json:"configMapGenerator"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	if got := kustomization.GeneratorOptions.Labels["grafana_dashboard"]; got != "1" {
		t.Errorf("grafana_dashboard label = %q, want \"1\" so the Grafana sidecar provisions the dashboard", got)
	}
	generators := make(map[string][]string, len(kustomization.ConfigMapGenerator))
	for _, generator := range kustomization.ConfigMapGenerator {
		generators[generator.Name] = generator.Files
	}
	if files, ok := generators["pagerless-dashboard"]; !ok || !slices.Contains(files, "pagerless.json") {
		t.Errorf("generators = %v, want a pagerless-dashboard ConfigMap from pagerless.json", generators)
	}
	if files := generators["pagerless-workload-dashboards"]; !slices.Equal(files,
		[]string{"workload-metrics.json", "workload-logs.json"}) {
		t.Errorf("workload dashboard files = %v, want the metrics and logs dashboards", files)
	}

	raw, err = os.ReadFile(filepath.Join(observabilityDir, "dashboards", "pagerless.json"))
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
	if dashboard.UID != pagerlessName || dashboard.Title != "Pagerless" {
		t.Errorf("dashboard = %q (%s), want the Pagerless dashboard", dashboard.Title, dashboard.UID)
	}
	if len(dashboard.Panels) < 6 {
		t.Errorf("dashboard has %d panels, want one for each exported metric", len(dashboard.Panels))
	}
	for _, name := range []string{
		metrics.MetricTriages,
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

// TestWorkloadDashboardsMatchTheServerDefaults checks that the dashboards the deployment provisions are the ones
// the dashboard API links to, so an embedded panel is never empty because of a UID mismatch.
func TestWorkloadDashboardsMatchTheServerDefaults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		file string
		uid  string
		want int
	}{
		{file: "workload-metrics.json", uid: server.DefaultMetricsDashboardUID, want: 4},
		{file: "workload-logs.json", uid: server.DefaultLogsDashboardUID, want: 2},
	} {
		t.Run(test.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(observabilityDir, "dashboards", test.file))
			if err != nil {
				t.Fatal(err)
			}
			var dashboard struct {
				UID        string            `json:"uid"`
				Title      string            `json:"title"`
				Panels     []json.RawMessage `json:"panels"`
				Templating struct {
					List []struct {
						Name string `json:"name"`
					} `json:"list"`
				} `json:"templating"`
			}
			if err := json.Unmarshal(raw, &dashboard); err != nil {
				t.Fatalf("the dashboard is not valid JSON: %v", err)
			}
			if dashboard.UID != test.uid {
				t.Errorf("uid = %q, want %q", dashboard.UID, test.uid)
			}
			if dashboard.Title == "" {
				t.Error("the dashboard has no title")
			}
			if len(dashboard.Panels) != test.want {
				t.Errorf("panels = %d, want %d", len(dashboard.Panels), test.want)
			}
			variables := make([]string, 0, len(dashboard.Templating.List))
			for _, variable := range dashboard.Templating.List {
				variables = append(variables, variable.Name)
			}
			if !slices.Equal(variables, []string{"namespace", "workload"}) {
				t.Errorf("variables = %v, want namespace and workload", variables)
			}
		})
	}
}

func TestServiceMonitorScrapesTheServerMetrics(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(observabilityDir, "monitors", "pagerless-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var monitor struct {
		Spec struct {
			NamespaceSelector struct {
				MatchNames []string `json:"matchNames"`
			} `json:"namespaceSelector"`
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Endpoints []struct {
				Port string `json:"port"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &monitor); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(monitor.Spec.NamespaceSelector.MatchNames, []string{pagerlessName}) {
		t.Errorf("namespaceSelector = %v, want the pagerless namespace", monitor.Spec.NamespaceSelector.MatchNames)
	}
	if got := monitor.Spec.Selector.MatchLabels["app.kubernetes.io/name"]; got != serverName {
		t.Errorf("selector = %v, want the pagerless-server Service", monitor.Spec.Selector.MatchLabels)
	}
	if len(monitor.Spec.Endpoints) != 1 || monitor.Spec.Endpoints[0].Port != "metrics" {
		t.Errorf("endpoints = %+v, want the metrics port", monitor.Spec.Endpoints)
	}
}

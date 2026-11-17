package server_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

type applicationSource struct {
	RepoURL        string `json:"repoURL"`
	Chart          string `json:"chart"`
	TargetRevision string `json:"targetRevision"`
	Ref            string `json:"ref"`
	Helm           struct {
		ReleaseName string   `json:"releaseName"`
		ValueFiles  []string `json:"valueFiles"`
	} `json:"helm"`
}

const (
	chaosMeshName        = "chaos-mesh"
	chaosMeshChartRepo   = "https://charts.chaos-mesh.org"
	platformRepoURL      = "https://github.com/a-hagi613/pagerless"
	platformMainRevision = "main"
)

type chaosMeshApplication struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Sources     []applicationSource `json:"sources"`
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
}

// The chart version pin is the single place that decides which Chaos Mesh runs,
// so a test reads the Application that the root Application renders.
func TestTheChaosMeshApplicationPinsTheChartAndItsValues(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../deploy/apps/templates/chaos-mesh.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"Values": map[string]any{
			"platform": map[string]any{
				"repoURL":        platformRepoURL,
				"targetRevision": platformMainRevision,
			},
		},
	}
	funcs := template.FuncMap{
		"quote": strconv.Quote,
		"required": func(message string, value any) (any, error) {
			if value == nil || value == "" {
				t.Fatalf("%s", message)
			}
			return value, nil
		},
	}
	tmpl, err := template.New("chaos-mesh.yaml").Funcs(funcs).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		t.Fatal(err)
	}
	var application chaosMeshApplication
	if err := yaml.Unmarshal(rendered.Bytes(), &application); err != nil {
		t.Fatalf("decode the rendered Application: %v", err)
	}

	if application.Metadata.Name != chaosMeshName || application.Metadata.Namespace != "argocd" {
		t.Errorf(
			"the Application is %s/%s, want argocd/chaos-mesh",
			application.Metadata.Namespace, application.Metadata.Name,
		)
	}
	if len(application.Spec.Sources) != 2 {
		t.Fatalf("the Application has %d sources, want the chart and the values repository", len(application.Spec.Sources))
	}
	chart, values := application.Spec.Sources[0], application.Spec.Sources[1]
	if chart.RepoURL != chaosMeshChartRepo || chart.Chart != chaosMeshName {
		t.Errorf("the chart source is %s/%s", chart.RepoURL, chart.Chart)
	}
	if chart.TargetRevision != "2.8.8" {
		t.Errorf("the chart targetRevision is %q, want the pinned 2.8.8", chart.TargetRevision)
	}
	if chart.Helm.ReleaseName != chaosMeshName {
		t.Errorf("the release name is %q", chart.Helm.ReleaseName)
	}
	const chaosValuesFile = "$values/deploy/chaos-mesh/values.yaml"
	if values.Ref != "values" || !slices.Equal(chart.Helm.ValueFiles, []string{chaosValuesFile}) {
		t.Fatalf("the chart does not read its values from %s: %+v", chaosValuesFile, chart.Helm.ValueFiles)
	}
	if values.RepoURL != platformRepoURL || values.TargetRevision != platformMainRevision {
		t.Errorf("the values source is %s at %s", values.RepoURL, values.TargetRevision)
	}
	if application.Spec.Destination.Namespace != chaosMeshName {
		t.Errorf("the Application destination is %q", application.Spec.Destination.Namespace)
	}

	// The values file the Application references must hold the containerd
	// settings that a kind node needs, and the matching image tag.
	path := filepath.Join("../../", strings.TrimPrefix(chaosValuesFile, "$values/"))
	valuesRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var settings struct {
		Images struct {
			Tag string `json:"tag"`
		} `json:"images"`
		ChaosDaemon struct {
			Runtime    string `json:"runtime"`
			SocketPath string `json:"socketPath"`
		} `json:"chaosDaemon"`
	}
	if err := yaml.Unmarshal(valuesRaw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Images.Tag != "v2.8.8" {
		t.Errorf("the Chaos Mesh image tag is %q, want v2.8.8", settings.Images.Tag)
	}
	const kindContainerdSocket = "/run/containerd/containerd.sock"
	if settings.ChaosDaemon.Runtime != "containerd" ||
		settings.ChaosDaemon.SocketPath != kindContainerdSocket {
		t.Errorf(
			"the chaos daemon uses runtime %q at %q, want containerd at the kind socket",
			settings.ChaosDaemon.Runtime, settings.ChaosDaemon.SocketPath,
		)
	}
}

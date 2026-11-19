package testenv

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The chart is the only install unit, so the harness renders it instead of
// keeping a second copy of the manifests that a cluster would drift from. The
// rendering is done once per test process and shared: every envtest server
// starts empty, so the same files can be applied to each of them.
var (
	chartOnce   sync.Once
	chartFiles  renderedChart
	chartRender error
)

type renderedChart struct {
	// objects is the file that holds the chart's objects, other than the CRDs,
	// which envtest installs from charts/pagerless/crds.
	objects string
}

// chart returns the rendered chart for the repository rooted at root.
func chart(t testing.TB, root string) renderedChart {
	t.Helper()
	chartOnce.Do(func() { chartFiles, chartRender = renderChart(root) })
	if chartRender != nil {
		t.Fatalf("render charts/pagerless: %v", chartRender)
	}
	return chartFiles
}

// renderChart runs helm template and keeps the objects the harness applies.
func renderChart(root string) (renderedChart, error) {
	if _, err := exec.LookPath("helm"); err != nil {
		return renderedChart{}, fmt.Errorf("helm is required by the envtest harness: %w", err)
	}
	var stdout, stderr bytes.Buffer
	command := exec.Command("helm", "template", "pagerless", filepath.Join(root, "charts", "pagerless"),
		"--namespace", Namespace,
		"--set", "credentials.secretName=pagerless-credentials")
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return renderedChart{}, fmt.Errorf("helm template: %w: %s", err, stderr.String())
	}
	objects, err := renderedObjects(stdout.String())
	if err != nil {
		return renderedChart{}, err
	}
	dir, err := os.MkdirTemp("", "pagerless-chart-")
	if err != nil {
		return renderedChart{}, fmt.Errorf("create the rendered chart directory: %w", err)
	}
	files := renderedChart{objects: filepath.Join(dir, "objects.yaml")}
	if err := os.WriteFile(files.objects, []byte(objects), 0o600); err != nil {
		return renderedChart{}, fmt.Errorf("write the rendered objects: %w", err)
	}
	return files, nil
}

// renderedObjects keeps the documents that the API server applies, dropping the
// CRDs, which the harness installs from charts/pagerless/crds.
func renderedObjects(rendered string) (string, error) {
	var objectDocs []string
	for document := range strings.SplitSeq(rendered, "\n---") {
		switch {
		case strings.TrimSpace(document) == "":
			continue
		case documentKind(document) == "CustomResourceDefinition":
			continue
		default:
			objectDocs = append(objectDocs, document)
		}
	}
	if len(objectDocs) == 0 {
		return "", fmt.Errorf("the chart rendered no objects")
	}
	return strings.Join(objectDocs, "\n---\n"), nil
}

// documentKind returns the kind of one YAML document, or an empty string.
func documentKind(document string) string {
	for line := range strings.SplitSeq(document, "\n") {
		if kind, ok := strings.CutPrefix(line, "kind: "); ok {
			return strings.TrimSpace(kind)
		}
	}
	return ""
}

// ChartObjects returns the file that holds the chart's rendered objects, for a
// test that asserts what the chart installs.
func ChartObjects(t testing.TB) string {
	t.Helper()
	return chart(t, repoRoot(t)).objects
}

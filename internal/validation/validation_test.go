package validation_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/validation"
)

// fixtureRepo is the base commit held in memory.
type fixtureRepo struct {
	files map[string]string
}

func newRepo(files map[string]string) *fixtureRepo { return &fixtureRepo{files: files} }

func (r *fixtureRepo) File(_ context.Context, _, path string) (validation.Blob, error) {
	content, ok := r.files[path]
	if !ok {
		return validation.Blob{}, errors.New("not found")
	}
	return validation.Blob{SHA: blobSHA(content), Content: []byte(content)}, nil
}

// blobSHA returns a deterministic Git-like blob identity for fixture content.
func blobSHA(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// testFile is one whole-file change with the content that the server holds.
type testFile struct {
	Edit    v1alpha1.FileEdit
	Content []byte
}

// edit returns the whole-file change for a path with the content.
func edit(repo *fixtureRepo, path, content string) testFile {
	base, ok := repo.files[path]
	if !ok {
		base = ""
	}
	sum := sha256.Sum256([]byte(content))
	return testFile{
		Edit: v1alpha1.FileEdit{
			Path: path, BaseBlob: blobSHA(base), ContentDigest: hex.EncodeToString(sum[:]),
		},
		Content: []byte(content),
	}
}

// deployment returns a Deployment that the recorded schema and the policy accept.
func deployment(name, image string, replicas int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: shop
spec:
  replicas: %d
  selector:
    matchLabels:
      app: checkout
  template:
    metadata:
      labels:
        app: checkout
    spec:
      serviceAccountName: checkout
      containers:
        - name: app
          image: %s
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
`, name, replicas, image)
}

// openAPIServer serves the recorded OpenAPI documents over HTTP.
func openAPIServer(t *testing.T) *validation.OpenAPI {
	t.Helper()
	dir := filepath.Join("testdata", "openapi")
	index, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	apps, err := os.ReadFile(filepath.Join(dir, "apps-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi/v3", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(index)
	})
	mux.HandleFunc("GET /openapi/v3/apis/apps/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(apps)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return validation.NewOpenAPI(server.URL, server.Client())
}

// validate runs Validation and fails the test on an infrastructure error.
func validate(t *testing.T, repo *fixtureRepo, files ...testFile) validation.Result {
	t.Helper()
	edits := make([]v1alpha1.FileEdit, 0, len(files))
	contents := make(map[string][]byte, len(files))
	for _, file := range files {
		edits = append(edits, file.Edit)
		contents[file.Edit.Path] = file.Content
	}
	result, err := validation.New(openAPIServer(t)).Validate(context.Background(), validation.Request{
		BaseCommit:    "base",
		Files:         edits,
		Contents:      contents,
		Repository:    repo,
		ValuesSchemas: validation.RepositoryValues{Repository: repo},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return result
}

// requireRefusal checks that Validation refused the Proposal with a message
// containing every fragment.
func requireRefusal(t *testing.T, result validation.Result, fragments ...string) {
	t.Helper()
	if result.Passed {
		t.Fatalf("Validation passed, want a refusal: %+v", result)
	}
	var joined []string
	for _, refusal := range result.Errors {
		joined = append(joined, refusal.Field+": "+refusal.Message)
	}
	all := strings.Join(joined, "\n")
	for _, fragment := range fragments {
		if !strings.Contains(all, fragment) {
			t.Errorf("refusals %q do not contain %q", all, fragment)
		}
	}
}

func TestRefusesWhatThePolicyForbids(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	cases := []struct {
		name      string
		path      string
		base      string
		proposed  string
		fragments []string
	}{
		{
			name: "github workflows", path: ".github/workflows/ci.yaml",
			base: "on: push\n", proposed: "on: pull_request\n",
			fragments: []string{"repository automation"},
		},
		{
			name: "secret", path: "apps/checkout/secret.yaml",
			base:      "apiVersion: v1\nkind: Secret\nmetadata:\n  name: checkout\n  namespace: shop\ndata:\n  token: aGk=\n",
			proposed:  "apiVersion: v1\nkind: Secret\nmetadata:\n  name: checkout\n  namespace: shop\ndata:\n  token: bm8=\n",
			fragments: []string{"kind Secret is always refused"},
		},
		{
			name: "cluster role", path: "apps/checkout/clusterrole.yaml",
			base:      "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: checkout\nrules: []\n",
			proposed:  "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: checkout\nrules:\n  - apiGroups: [\"\"]\n    resources: [pods]\n    verbs: [get]\n",
			fragments: []string{"kind ClusterRole is always refused"},
		},
		{
			name: "custom resource definition", path: "apps/checkout/crd.yaml",
			base:      "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n",
			proposed:  "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\nspec: {}\n",
			fragments: []string{"kind CustomResourceDefinition is always refused"},
		},
		{
			name: "webhook configuration", path: "apps/checkout/webhook.yaml",
			base:      "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\nmetadata:\n  name: checkout\nwebhooks: []\n",
			proposed:  "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\nmetadata:\n  name: checkout\nwebhooks:\n  - name: check.example.com\n",
			fragments: []string{"kind ValidatingWebhookConfiguration is always refused"},
		},
		{
			name: "host path", path: manifestPath,
			base: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
			proposed: strings.Replace(deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2), "    spec:\n",
				"    spec:\n      volumes:\n        - name: host\n          hostPath:\n            path: /var/run\n", 1),
			fragments: []string{"mounts a hostPath"},
		},
		{
			name: "privileged container", path: manifestPath,
			base: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
			proposed: strings.Replace(deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2), "          image:",
				"          securityContext:\n            privileged: true\n          image:", 1),
			fragments: []string{"securityContext.privileged is forbidden"},
		},
		{
			name: "changed selector", path: manifestPath,
			base:      deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
			proposed:  strings.Replace(deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2), "app: checkout", "app: checkout-v2", 1),
			fragments: []string{"spec.selector may not change"},
		},
		{
			name: "changed name", path: manifestPath,
			base:      deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
			proposed:  deployment("checkout-old", "ghcr.io/acme/checkout:1.0.0", 2),
			fragments: []string{"changes the document identity"},
		},
		{
			name: "emptied file", path: manifestPath,
			base: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2), proposed: "",
			fragments: []string{"holds no Kubernetes document"},
		},
		{
			name: "service account", path: manifestPath,
			base:      deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
			proposed:  strings.Replace(deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2), "serviceAccountName: checkout", "serviceAccountName: admin", 1),
			fragments: []string{"serviceAccountName may not change"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newRepo(map[string]string{tc.path: tc.base})
			result := validate(t, repo, edit(repo, tc.path, tc.proposed))
			requireRefusal(t, result, tc.fragments...)
		})
	}
}

func TestRefusesNewFilesAndTooManyFiles(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{})
	newFile := edit(repo, "apps/checkout/new.yaml", deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2))
	requireRefusal(t, validate(t, repo, newFile), "is not a file at the base commit")

	files := map[string]string{}
	var edits []testFile
	for index := range 6 {
		path := fmt.Sprintf("apps/app%d/deployment.yaml", index)
		content := deployment(fmt.Sprintf("app%d", index), "ghcr.io/acme/app:1.0.0", 2)
		files[path] = content
		repo.files[path] = content
		edits = append(edits, edit(repo, path, strings.Replace(content, "replicas: 2", "replicas: 3", 1)))
	}
	requireRefusal(t, validate(t, repo, edits...), "at most 5 files")
}

func TestRefusesAnOversizedFileAndDiff(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	repo := newRepo(map[string]string{manifestPath: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2)})
	big := strings.Replace(deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2),
		"metadata:\n", "metadata:\n  annotations:\n    note: \""+strings.Repeat("a", 65<<10)+"\"\n", 1)
	requireRefusal(t, validate(t, repo, edit(repo, manifestPath, big)), "above the 65536-byte limit")

	base := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: checkout\n  namespace: shop\ndata:\n"
	var proposed strings.Builder
	proposed.WriteString(base)
	for index := range 900 {
		fmt.Fprintf(&proposed, "  key%04d: %s\n", index, strings.Repeat("a", 45))
	}
	repo = newRepo(map[string]string{"apps/checkout/config.yaml": base})
	result := validate(t, repo, edit(repo, "apps/checkout/config.yaml", proposed.String()))
	requireRefusal(t, result, "above the 32768-byte limit")
}

func TestAcceptsABoundedChangeWithWarnings(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	base := deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2)
	proposed := deployment("checkout", "ghcr.io/acme/checkout:1.1.0", 3)
	proposed = strings.Replace(proposed, "memory: 256Mi", "memory: 512Mi", 1)
	repo := newRepo(map[string]string{manifestPath: base})
	result := validate(t, repo, edit(repo, manifestPath, proposed))
	if !result.Passed {
		t.Fatalf("Validation refused a bounded change: %+v", result.Errors)
	}
	classes := map[v1alpha1.ProposalWarningClass]bool{}
	for _, warning := range result.Warnings {
		classes[warning.Class] = true
	}
	if !classes[v1alpha1.WarningImage] || !classes[v1alpha1.WarningReplicas] {
		t.Errorf("warnings = %+v, want an image and a replica warning", result.Warnings)
	}
	if got := result.Validation().Admission; got != v1alpha1.AdmissionNotEvaluated {
		t.Errorf("admission = %q, want %q", got, v1alpha1.AdmissionNotEvaluated)
	}
	if len(result.Diffs) != 1 || !strings.Contains(result.Diffs[0].Diff, "1.1.0") {
		t.Errorf("diffs = %+v, want the derived diff", result.Diffs)
	}
}

func TestZeroReplicasIsPermittedAndFlagged(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	repo := newRepo(map[string]string{manifestPath: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2)})
	result := validate(t, repo, edit(repo, manifestPath, deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 0)))
	if !result.Passed {
		t.Fatalf("Validation refused zero replicas: %+v", result.Errors)
	}
	flagged := false
	for _, warning := range result.Warnings {
		if warning.Class == v1alpha1.WarningReplicasZero {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("warnings = %+v, want a ReplicasZero warning", result.Warnings)
	}
}

func TestOpenAPIRejectsAnUnknownContainerField(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	base := deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2)
	broken := strings.Replace(base, "          image:", "          resource:\n            limits:\n              memory: 1Gi\n          image:", 1)
	repo := newRepo(map[string]string{manifestPath: base})
	requireRefusal(t, validate(t, repo, edit(repo, manifestPath, broken)), "unknown field", "resource")

	fixed := strings.Replace(base, "replicas: 2", "replicas: 3", 1)
	result := validate(t, repo, edit(repo, manifestPath, fixed))
	if !result.Passed {
		t.Fatalf("the corrected manifest was refused: %+v", result.Errors)
	}
}

func TestCustomResourceWithoutASchemaDegrades(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/widget.yaml"
	base := "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: checkout\n  namespace: shop\nspec:\n  size: 1\n"
	proposed := strings.Replace(base, "size: 1", "size: 2", 1)
	repo := newRepo(map[string]string{manifestPath: base})
	result := validate(t, repo, edit(repo, manifestPath, proposed))
	if !result.Passed {
		t.Fatalf("Validation refused a degraded Proposal: %+v", result.Errors)
	}
	if !result.Degraded {
		t.Error("Degraded = false, want true: the group and version have no schema")
	}
	degraded := false
	for _, warning := range result.Warnings {
		if warning.Class == v1alpha1.WarningDegraded {
			degraded = true
		}
	}
	if !degraded {
		t.Errorf("warnings = %+v, want a Degraded warning", result.Warnings)
	}
}

func TestHelmValuesAreCheckedAgainstTheChartSchema(t *testing.T) {
	t.Parallel()
	const valuesPath = "charts/app/values.yaml"
	const schemaPath = "charts/app/values.schema.json"
	repo := newRepo(map[string]string{
		valuesPath: "replicas: 2\n",
		schemaPath: `{"type":"object","properties":{"replicas":{"type":"integer","minimum":1}},` +
			`"required":["replicas"],"additionalProperties":false}`,
	})
	requireRefusal(t, validate(t, repo, edit(repo, valuesPath, "replicas: 0\n")),
		"values.schema.json refuses the values", "minimum")

	result := validate(t, repo, edit(repo, valuesPath, "replicas: 3\n"))
	if !result.Passed {
		t.Fatalf("the valid values were refused: %+v", result.Errors)
	}
}

func TestBaseBlobAndDigestMustMatch(t *testing.T) {
	t.Parallel()
	const manifestPath = "apps/checkout/deployment.yaml"
	repo := newRepo(map[string]string{manifestPath: deployment("checkout", "ghcr.io/acme/checkout:1.0.0", 2)})
	file := edit(repo, manifestPath, deployment("checkout", "ghcr.io/acme/checkout:1.1.0", 2))
	file.Edit.BaseBlob = blobSHA("something else")
	requireRefusal(t, validate(t, repo, file), "is", "at the base commit, not")

	file = edit(repo, manifestPath, deployment("checkout", "ghcr.io/acme/checkout:1.1.0", 2))
	file.Edit.ContentDigest = strings.Repeat("0", 64)
	requireRefusal(t, validate(t, repo, file), "does not match its digest")
}

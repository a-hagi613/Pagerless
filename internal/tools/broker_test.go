package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// The fixture's stable strings, shared by the broker tests.
const (
	testAPIVersion   = "apps/v1"
	testWorkload     = "checkout"
	testReason       = "BackOff"
	testManifestPath = "apps/shop/checkout.yaml"
	testPodName      = "checkout-6d9f8c7b5-2xk4p"
	testConnector    = "loki"
	testRef          = "main"
	testNamespace    = "shop"
	testKilledLine   = "Killed"
	testTargetUID    = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

// pageFixture is the Page every broker test investigates: a Deployment in shop
// whose pods carry the checkout name.
func pageFixture() *v1alpha1.Page {
	page := &v1alpha1.Page{Spec: v1alpha1.PageSpec{
		Target: v1alpha1.TargetReference{
			APIVersion: testAPIVersion, Kind: v1alpha1.TargetKindDeployment,
			Namespace: testNamespace, Name: testWorkload, UID: testTargetUID,
		},
		Fingerprint: strings.Repeat("a", 64),
		Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent, Reason: testReason},
		Repository: &v1alpha1.RepositoryReference{
			URL: "https://github.com/acme/gitops", Branch: testRef, Path: testManifestPath,
		},
		Hint: "We deployed at 14:02.",
	}}
	page.Name, page.Namespace, page.UID = "checkout-oom", "pagerless", "0f3a1b2c-1111-4222-8333-444455556666"
	return page
}

// objects returns the cluster the broker reads: the Target, its pods, a Secret,
// a Node, another workload, and an Event.
func objects() []runtime.Object {
	deployment := &appsv1.Deployment{}
	deployment.Name, deployment.Namespace, deployment.UID = testWorkload, testNamespace, types.UID(testTargetUID)
	replicaSet := &appsv1.ReplicaSet{}
	replicaSet.Name, replicaSet.Namespace, replicaSet.UID = "checkout-6d9f8c7b5", testNamespace, "rs-uid"
	replicaSet.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: testAPIVersion, Kind: kindDeployment, Name: testWorkload, UID: types.UID(testTargetUID),
		Controller: new(true),
	}}
	owned := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	owned.Name, owned.Namespace, owned.UID = testPodName, testNamespace, "pod-uid"
	owned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: testAPIVersion, Kind: kindReplicaSet, Name: "checkout-6d9f8c7b5", UID: "rs-uid",
		Controller: new(true),
	}}
	other := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	other.Name, other.Namespace = "cart-0", testNamespace
	secret := &corev1.Secret{Data: map[string][]byte{"password": []byte("hunter2")}}
	secret.Name, secret.Namespace = "db", testNamespace
	node := &corev1.Node{}
	node.Name = "node-1"
	event := &corev1.Event{
		InvolvedObject: corev1.ObjectReference{
			Kind: kindDeployment, Name: testWorkload, Namespace: testNamespace, UID: types.UID(testTargetUID),
		},
		Type: "Warning", Reason: testReason, Message: "Back-off restarting failed container",
		LastTimestamp: metav1.NewTime(time.Date(2026, 11, 18, 9, 58, 0, 0, time.UTC)),
	}
	event.Name, event.Namespace = "checkout.1", testNamespace
	job := &batchv1.Job{}
	job.Name, job.Namespace = "unrelated", testNamespace
	return []runtime.Object{deployment, replicaSet, owned, other, secret, node, event, job}
}

// fakeLogs returns deterministic pod logs and records what was asked for.
type fakeLogs struct {
	previous map[string]string
}

func (f *fakeLogs) GetLogs(
	_ context.Context, _, pod string, options *corev1.PodLogOptions,
) ([]byte, error) {
	if options.Previous {
		return []byte(f.previous[pod]), nil
	}
	return []byte("current line\n"), nil
}

// fakeGitHub records repository reads and answers with a fixed tree and file.
type fakeGitHub struct {
	repository Repository
	ref        string
	requests   []string
	bigFile    bool
}

func (f *fakeGitHub) Tree(_ context.Context, repository Repository, ref, directory string) (Tree, error) {
	f.repository, f.ref = repository, ref
	f.requests = append(f.requests, "tree:"+directory)
	return Tree{Path: directory, Entries: []TreeEntry{{Path: testManifestPath, Type: "blob", Size: 10}}}, nil
}

func (f *fakeGitHub) File(_ context.Context, repository Repository, ref, path string) (File, error) {
	f.repository, f.ref = repository, ref
	f.requests = append(f.requests, "file:"+path)
	if f.bigFile {
		return File{Path: path, Ref: ref, Size: maxRepoFileBytes, Content: strings.Repeat("a", maxRepoFileBytes)}, nil
	}
	return File{Path: path, Ref: ref, Size: 12, Content: "kind: Deployment\n"}, nil
}

func (f *fakeGitHub) Commits(
	_ context.Context, repository Repository, ref, path string, limit int,
) ([]Commit, error) {
	f.repository, f.ref = repository, ref
	f.requests = append(f.requests, "commits:"+path)
	return []Commit{{SHA: "3f2a9c1", Message: "raise the memory limit"}}, nil
}

func (f *fakeGitHub) Head(_ context.Context, repository Repository, ref string) (string, error) {
	f.repository, f.ref = repository, ref
	return "3f2a9c1", nil
}

// fakeConnectors is a configured Connector catalogue that records each query.
type fakeConnectors struct {
	queries []LogQuery
	names   []string
}

func (f *fakeConnectors) Names() []string {
	if f.names == nil {
		return []string{testConnector}
	}
	return f.names
}

func (f *fakeConnectors) Logs(_ context.Context, connector string, query LogQuery) (LogPage, error) {
	if connector != testConnector {
		return LogPage{}, context.Canceled
	}
	f.queries = append(f.queries, query)
	return LogPage{Entries: []LogEntry{{Line: testKilledLine, Severity: "error"}}}, nil
}

func (f *fakeConnectors) Metrics(_ context.Context, _ string, _ MetricQuery) ([]MetricSeries, error) {
	return []MetricSeries{{Labels: map[string]string{"pod": testWorkload}, Points: []MetricPoint{{
		Timestamp: time.Date(2026, 11, 18, 9, 58, 0, 0, time.UTC), Value: 1,
	}}}}, nil
}

func (f *fakeConnectors) Health(_ context.Context, connector string) (ConnectorHealth, error) {
	return ConnectorHealth{Name: connector, Kind: testConnector, Enabled: true}, nil
}

// brokerFixture builds a broker over the fixture cluster.
func brokerFixture(t *testing.T) (*Broker, *MemoryStore, *fakeConnectors, *fakeGitHub) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects()...).Build()
	store := NewMemoryStore()
	connectors := &fakeConnectors{}
	github := &fakeGitHub{}
	broker := &Broker{
		Reader: reader,
		PodLogs: &fakeLogs{previous: map[string]string{
			testPodName: "allocating 512 MiB\nKilled\n",
		}},
		GitHub:     github,
		Connectors: connectors,
		Store:      store,
		Clock:      clocktesting.NewFakeClock(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)),
		Evidence:   "memory",
	}
	return broker, store, connectors, github
}

// call runs one tool call and fails the test on a transport error.
func call(
	t *testing.T, broker *Broker, page *v1alpha1.Page, tool, key string, arguments map[string]any,
) (Result, *Refusal) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	result, refused, _ := broker.Call(t.Context(), page, tool, raw, key)
	return result, refused
}

// TestTheScopeRefusesEveryReadOutsideThePage covers each refusal class.
func TestTheScopeRefusesEveryReadOutsideThePage(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, connectors, _ := brokerFixture(t)
	cases := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{"another namespace", ToolObjectGet, map[string]any{fieldKind: kindPod, "name": "owned", "namespace": "other"}},
		{"another repository", ToolRepoFile, map[string]any{fieldPath: testManifestPath, "repository": "evil/gitops"}},
		{"a Secret", ToolObjectGet, map[string]any{fieldKind: "Secret", fieldName: "db"}},
		{"a cluster-scoped kind", ToolObjectGet, map[string]any{fieldKind: "Node", fieldName: "node-1"}},
		{"a pod outside the Target", ToolPodsLogs, map[string]any{"pod": "cart-0"}},
		{"a Connector outside the Page", ToolConnectorLogs, map[string]any{"connector": "prometheus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, refused := call(t, broker, page, tc.tool, "key-"+tc.name, tc.arguments)
			if refused == nil {
				t.Fatalf("%s was served, want out_of_scope", tc.name)
			}
			if refused.Error.Code != CodeOutOfScope {
				t.Errorf("code = %q, want %q (%s)", refused.Error.Code, CodeOutOfScope, refused.Error.Message)
			}
		})
	}
	_ = connectors
}

// TestAConnectorQueryCannotWidenTheNamespaceOrTenant checks the server-side
// injection: a model-supplied namespace and tenant are ignored, and the
// Connector sees the Page's namespace and Target.
func TestAConnectorQueryCannotWidenTheNamespaceOrTenant(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, connectors, _ := brokerFixture(t)
	result, refused := call(t, broker, page, ToolConnectorLogs, "key", map[string]any{
		"connector": testConnector, "namespace": "kube-system", "tenant": "someone-else", "query": "{}",
	})
	if refused != nil {
		t.Fatalf("the query was refused: %+v", refused)
	}
	if !result.OK || len(connectors.queries) != 1 {
		t.Fatalf("result = %+v, queries = %d, want one Connector query", result, len(connectors.queries))
	}
	query := connectors.queries[0]
	if query.Namespace != testNamespace {
		t.Errorf("Connector namespace = %q, want the Page's namespace shop", query.Namespace)
	}
	if query.Subject.Name != testWorkload || query.Subject.Kind != v1alpha1.TargetKindDeployment {
		t.Errorf("Connector subject = %+v, want the Page's Target", query.Subject)
	}
}

// TestTheManifestCarriesTheBundleCatalogueScopeAndLimits covers the one-call
// manifest.
func TestTheManifestCarriesTheBundleCatalogueAndLimits(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	broker.Context = contextSource(fmt.Sprintf(`{"target":{"kind":%q}}`, kindDeployment))
	manifest, err := broker.Manifest(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Page.UID != string(page.UID) || manifest.Page.Name != page.Name {
		t.Errorf("manifest Page = %+v, want the Page's identity", manifest.Page)
	}
	if string(manifest.Context) != fmt.Sprintf(`{"target":{"kind":%q}}`, kindDeployment) {
		t.Errorf("Context = %s, want the stored bundle", manifest.Context)
	}
	if manifest.Limits.ToolCalls != 15 || manifest.Limits.ModelCalls != 12 ||
		manifest.Limits.InputTokens != 300_000 || manifest.Limits.Seconds != 300 {
		t.Errorf("limits = %+v, want the four documented budgets", manifest.Limits)
	}
	if len(manifest.Scope.Namespaces) != 1 || manifest.Scope.Namespaces[0] != testNamespace {
		t.Errorf("namespaces = %v, want only shop", manifest.Scope.Namespaces)
	}
	if len(manifest.Scope.Repositories) != 1 ||
		manifest.Scope.Repositories[0] != (Repository{Owner: "acme", Name: "gitops", Ref: testRef}) {
		t.Errorf("repositories = %v, want acme/gitops at main", manifest.Scope.Repositories)
	}
	if len(manifest.Scope.Connectors) != 1 || manifest.Scope.Connectors[0] != testConnector {
		t.Errorf("connectors = %v, want the configured catalogue", manifest.Scope.Connectors)
	}
	if len(manifest.Tools) != len(Names()) {
		t.Errorf("tools = %d, want the whole catalogue", len(manifest.Tools))
	}
	if manifest.Hint != "We deployed at 14:02." {
		t.Errorf("hint = %q, want the human Hint", manifest.Hint)
	}
}

// TestTheManifestCatalogueUsesTheFieldNamesTheAgentValidates pins the wire
// contract between the server's catalogue and the agent's Pydantic models. The
// agent validates the manifest with extra="forbid" and requires input_schema, so
// a camelCase key here fails every investigation before it starts - which is
// exactly what the first live cluster run produced. The assertion is made on the
// encoded manifest, not on the Go struct, so a tag change cannot slip past it.
func TestTheManifestCatalogueUsesTheFieldNamesTheAgentValidates(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	manifest, err := broker.Manifest(t.Context(), page)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Tools) == 0 {
		t.Fatal("the manifest carries no tools")
	}
	for _, tool := range decoded.Tools {
		var name string
		_ = json.Unmarshal(tool["name"], &name)
		if _, ok := tool["input_schema"]; !ok {
			t.Errorf("tool %q has no input_schema field; keys are the agent's contract", name)
		}
		if _, ok := tool["inputSchema"]; ok {
			t.Errorf("tool %q carries inputSchema, which the agent's contract forbids", name)
		}
	}
}

// TestTheCatalogueHasNoWriteTool asserts the fixed catalogue: every entry is a
// read tool, and the write verbs a model might try are absent.
func TestTheCatalogueHasNoWriteTool(t *testing.T) {
	t.Parallel()
	want := []string{
		"object_get", "object_list", "pods_logs", "events_list", "owner_chain",
		"connector_logs", "connector_metrics", "connector_health",
		"repo_tree", "repo_file", "repo_commits",
	}
	if got := Names(); len(got) != len(want) {
		t.Fatalf("catalogue = %v, want %v", got, want)
	}
	for i, name := range want {
		if Names()[i] != name {
			t.Errorf("catalogue[%d] = %q, want %q", i, Names()[i], name)
		}
		if inCatalogue("k8s_" + name) {
			t.Errorf("a write-shaped name %q is in the catalogue", "k8s_"+name)
		}
	}
	for _, forbidden := range []string{"delete_pod", "exec", "create", "patch", "apply", "kubectl"} {
		if inCatalogue(forbidden) {
			t.Errorf("the write tool %q is in the catalogue", forbidden)
		}
	}
}

// contextSource is a ContextSource that returns a fixed document.
type contextSource string

func (c contextSource) Context(context.Context, *v1alpha1.Page) (json.RawMessage, error) {
	return json.RawMessage(c), nil
}

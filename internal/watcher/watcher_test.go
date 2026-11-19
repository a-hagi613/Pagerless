package watcher

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
)

// namespaceReader returns a reader over the namespaces a test names.
func namespaceReader(t *testing.T, namespaces ...*corev1.Namespace) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add the core types to the scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, namespace := range namespaces {
		builder = builder.WithObjects(namespace)
	}
	return builder.Build()
}

func TestResolveClusterIDPrefersKubeSystem(t *testing.T) {
	t.Parallel()
	reader := namespaceReader(t,
		&corev1.Namespace{Name: kubeSystemNamespace, UID: types.UID("kube-uid")},
		&corev1.Namespace{Name: testReleaseNamespace, UID: types.UID("release-uid")},
	)
	got, err := ResolveClusterID(context.Background(), reader, testReleaseNamespace)
	if err != nil || got != "kube-uid" {
		t.Fatalf("cluster id = %q err=%v, want the kube-system UID", got, err)
	}
}

func TestResolveClusterIDFallsBackToTheReleaseNamespaceUID(t *testing.T) {
	t.Parallel()
	reader := namespaceReader(t,
		&corev1.Namespace{Name: testReleaseNamespace, UID: types.UID("release-uid")},
	)
	got, err := ResolveClusterID(context.Background(), reader, testReleaseNamespace)
	if err != nil || got != "release-uid" {
		t.Fatalf("cluster id = %q err=%v, want the release namespace UID", got, err)
	}
}

// TestResolveClusterIDFallsBackToTheNamespaceName covers the namespaced read
// Role of ADR-0038, which may not read any namespace object.
func TestResolveClusterIDFallsBackToTheNamespaceName(t *testing.T) {
	t.Parallel()
	reader := namespaceReader(t)
	got, err := ResolveClusterID(context.Background(), reader, testReleaseNamespace)
	if err != nil || got != "namespace/pagerless" {
		t.Fatalf("cluster id = %q err=%v, want a stable release-namespace identifier", got, err)
	}
	again, err := ResolveClusterID(context.Background(), reader, testReleaseNamespace)
	if err != nil || again != got {
		t.Fatalf("cluster id is not stable: %q then %q", got, again)
	}
	if _, err := ResolveClusterID(context.Background(), reader, ""); err == nil {
		t.Fatal("ResolveClusterID accepted an empty release namespace")
	}
}

// cacheEntry finds the configuration of one object type. The map is keyed by a
// prototype object, so a caller cannot index it with a fresh pointer.
func cacheEntry(m map[client.Object]cache.ByObject, probe client.Object) (cache.ByObject, bool) {
	for object, config := range m {
		if reflect.TypeOf(object) == reflect.TypeOf(probe) {
			return config, true
		}
	}
	return cache.ByObject{}, false
}

func TestCacheByObjectScopesTheAllowlist(t *testing.T) {
	t.Parallel()
	cached := CacheByObject([]string{testNamespace, testOtherNamespace}, ModeWatch)
	pod, ok := cacheEntry(cached, &corev1.Pod{})
	if !ok {
		t.Fatal("Pods are not cached")
	}
	if len(pod.Namespaces) != 2 {
		t.Fatalf("Pod namespaces = %v, want exactly the allowlist", pod.Namespaces)
	}
	for _, namespace := range []string{testNamespace, testOtherNamespace} {
		if _, ok := pod.Namespaces[namespace]; !ok {
			t.Errorf("namespace %q is not cached", namespace)
		}
	}
	if _, ok := pod.Namespaces[metav1.NamespaceAll]; ok {
		t.Error("a namespaced installation must not cache every namespace")
	}
	if _, ok := cacheEntry(cached, &corev1.Node{}); !ok {
		t.Error("Nodes are cached by default")
	}
	if _, ok := cacheEntry(cached, &appsv1.ReplicaSet{}); !ok {
		t.Error("ReplicaSets are cached, because a Pod resolves to its Deployment through one")
	}

	clusterWide := CacheByObject(nil, ModeWatch)
	clusterPods, _ := cacheEntry(clusterWide, &corev1.Pod{})
	if _, ok := clusterPods.Namespaces[metav1.NamespaceAll]; !ok {
		t.Error("the default cache must watch every namespace")
	}
	noNodes := CacheByObject(nil, ModeIgnore)
	if _, ok := cacheEntry(noNodes, &corev1.Node{}); ok {
		t.Error("a Node-ignoring installation must not cache Nodes")
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg := Config{Namespace: testReleaseNamespace}.withDefaults()
	if cfg.KubeSystemMode != ModeWarnOnly {
		t.Errorf("kube-system mode = %s, want warn-only", cfg.KubeSystemMode)
	}
	if cfg.NodePagesMode != ModeWatch {
		t.Errorf("Node mode = %s, want watch", cfg.NodePagesMode)
	}
	if cfg.MinSeverity != v1alpha1.SeverityWarning {
		t.Errorf("minimum severity = %s, want warning", cfg.MinSeverity)
	}
	if cfg.ClearFor != DefaultClearFor || cfg.EvaluationInterval != DefaultEvaluationInterval {
		t.Errorf("watcher intervals = %s / %s, want the defaults", cfg.ClearFor, cfg.EvaluationInterval)
	}
	if len(cfg.Ladder.Steps) != len(cooldown.DefaultSteps) {
		t.Errorf("ladder = %+v, want the specified one", cfg.Ladder)
	}
	if cfg.Clock == nil {
		t.Error("clock is nil, want the real clock")
	}
	if cfg.AggregationWindow != DefaultAggregationWindow || cfg.Burst != DefaultBurst {
		t.Errorf("gate window = %s burst = %d, want the defaults", cfg.AggregationWindow, cfg.Burst)
	}
}

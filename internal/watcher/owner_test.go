package watcher

import (
	"context"
	"errors"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// fakeCluster is a Lookup and ClaimConsumers backed by a map, so the
// canonicalizer can be tested without an API server.
type fakeCluster struct {
	objects   map[Reference]*Object
	consumers map[Reference][]Reference
}

// newFakeCluster returns an empty fake.
func newFakeCluster() *fakeCluster {
	return &fakeCluster{objects: map[Reference]*Object{}, consumers: map[Reference][]Reference{}}
}

// add registers an object owned by owner, which may be nil.
func (f *fakeCluster) add(kind, namespace, name, uid string, owner *Reference) *Object {
	object := &Object{
		Reference:     Reference{APIVersion: APIVersionFor(kind), Kind: kind, Namespace: namespace, Name: name, UID: uid},
		Annotations:   map[string]string{},
		Labels:        map[string]string{},
		ControllerRef: owner,
	}
	if owner != nil {
		object.ControllerRefs = 1
	}
	f.objects[object.Reference] = object
	return object
}

// Get returns a registered object.
func (f *fakeCluster) Get(_ context.Context, reference Reference) (*Object, error) {
	object, ok := f.objects[reference]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return object, nil
}

// Consumers returns the Pods registered as mounting a claim. The fake keys its
// table by namespace and claim name, because that is the identity the resolver
// asks for; the API version and UID of a claim never reach this lookup.
func (f *fakeCluster) Consumers(_ context.Context, namespace, claim string) ([]Reference, error) {
	for key, consumers := range f.consumers {
		if key.Namespace == namespace && key.Name == claim {
			return consumers, nil
		}
	}
	return nil, nil
}

// resolver returns a Resolver over the fake.
func (f *fakeCluster) resolver() *Resolver {
	return &Resolver{Lookup: f, Claims: f}
}

// ownerRef builds an owner reference from an object.
func ownerRef(object *Object) *Reference {
	reference := object.Reference
	return &reference
}

func TestResolverFollowsControllerOwners(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	deployment := cluster.add(testKindDeployment, testNamespace, testDeployment, testDeploymentUID, nil)
	replicaSet := cluster.add("ReplicaSet", testNamespace, "checkout-7d9f", "rs-1", ownerRef(deployment))
	pod := cluster.add(testKindPod, testNamespace, "checkout-7d9f-abc12", testPodUID, ownerRef(replicaSet))

	subject, err := cluster.resolver().Resolve(context.Background(), pod.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := v1alpha1.TargetReference{
		APIVersion: apiVersionApps,
		Kind:       v1alpha1.TargetKindDeployment,
		Namespace:  testNamespace,
		Name:       testDeployment,
		UID:        testDeploymentUID,
	}
	if subject.Target() != want {
		t.Fatalf("subject = %+v, want %+v", subject.Target(), want)
	}
	if subject.Resolution != v1alpha1.ResolutionCanonical {
		t.Fatalf("resolution = %s, want Canonical", subject.Resolution)
	}
}

func TestResolverStopsAtTheHighestSupportedObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		parent string
		child  string
	}{
		{name: "statefulset", parent: testKindStatefulSet, child: testKindPod},
		{name: "daemonset", parent: "DaemonSet", child: testKindPod},
		{name: "cronjob chain", parent: testKindCronJob, child: testKindPod},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cluster := newFakeCluster()
			owner := cluster.add(testCase.parent, testNamespace, "owner", "o-1", nil)
			current := owner
			if testCase.parent == testKindCronJob {
				current = cluster.add("Job", testNamespace, "nightly-1", "j-1", ownerRef(owner))
			}
			pod := cluster.add(testKindPod, testNamespace, "owner-abc", testPodUID, ownerRef(current))
			subject, err := cluster.resolver().Resolve(context.Background(), pod.Reference)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if subject.Kind != testCase.parent || subject.UID != "o-1" {
				t.Fatalf("subject = %s/%s, want %s/o-1", subject.Kind, subject.UID, testCase.parent)
			}
		})
	}
}

func TestResolverReportsPartialResolution(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*fakeCluster) *Object{
		"owner deleted": func(cluster *fakeCluster) *Object {
			missing := Reference{Kind: "ReplicaSet", Namespace: testNamespace, Name: "gone", UID: "rs-gone"}
			return cluster.add(testKindPod, testNamespace, "pod", testPodUID, &missing)
		},
		"owner uid changed": func(cluster *fakeCluster) *Object {
			replicaSet := cluster.add("ReplicaSet", testNamespace, "recreated", "rs-new", nil)
			stale := *ownerRef(replicaSet)
			stale.UID = "rs-old"
			return cluster.add(testKindPod, testNamespace, "pod", testPodUID, &stale)
		},
		"multiple controller refs": func(cluster *fakeCluster) *Object {
			deployment := cluster.add(testKindDeployment, testNamespace, "first", testDeploymentUID, nil)
			pod := cluster.add(testKindPod, testNamespace, "pod", testPodUID, ownerRef(deployment))
			pod.ControllerRefs = 2
			return pod
		},
		"cross namespace owner": func(cluster *fakeCluster) *Object {
			deployment := cluster.add(testKindDeployment, "other", testDeployment, testDeploymentUID, nil)
			return cluster.add(testKindPod, testNamespace, "pod", testPodUID, ownerRef(deployment))
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cluster := newFakeCluster()
			pod := build(cluster)
			subject, err := cluster.resolver().Resolve(context.Background(), pod.Reference)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if subject.Resolution != v1alpha1.ResolutionPartial {
				t.Fatalf("resolution = %s, want Partial", subject.Resolution)
			}
			if subject.Kind != testKindPod || subject.UID != testPodUID {
				t.Fatalf("subject = %s/%s, want the Pod", subject.Kind, subject.UID)
			}
		})
	}
}

func TestResolverDetectsACycle(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	first := cluster.add(testKindDeployment, testNamespace, "first", testDeploymentUID, nil)
	second := cluster.add(testKindDeployment, testNamespace, "second", testRecreatedUID, ownerRef(first))
	first.ControllerRef = ownerRef(second)
	first.ControllerRefs = 1

	subject, err := cluster.resolver().Resolve(context.Background(), first.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if subject.Resolution != v1alpha1.ResolutionPartial {
		t.Fatalf("resolution = %s, want Partial", subject.Resolution)
	}
	if subject.UID != testRecreatedUID {
		t.Fatalf("subject = %s, want the furthest supported object d-2", subject.UID)
	}
}

func TestResolverCapsTheDepth(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	// Each object owns the one below it, so the leaf carries the whole chain and
	// resolving it must stop at the depth cap rather than walk forever.
	current := cluster.add(testKindDeployment, testNamespace, "root", "d-0", nil)
	var leaf *Object
	for depth := 1; depth <= maxOwnerDepth+1; depth++ {
		leaf = cluster.add(testKindDeployment, testNamespace, "child", "d-"+string(rune('0'+depth)), ownerRef(current))
		current = leaf
	}
	subject, err := cluster.resolver().Resolve(context.Background(), leaf.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if subject.Resolution != v1alpha1.ResolutionPartial {
		t.Fatalf("resolution = %s, want Partial after the depth cap", subject.Resolution)
	}
	if subject.UID == leaf.Reference.UID {
		t.Fatal("the resolver stopped at the leaf instead of following the chain")
	}
	if subject.UID != testDeploymentUID {
		t.Fatalf("subject = %s, want the object one hop past the depth cap", subject.UID)
	}
}

func TestResolverIgnoresMirrorPods(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	pod := cluster.add(testKindPod, kubeSystemNamespace, "kube-apiserver-node-1", testPodUID, nil)
	pod.Annotations["kubernetes.io/config.mirror"] = "hash"
	if _, err := cluster.resolver().Resolve(context.Background(), pod.Reference); !errors.Is(err, ErrIgnored) {
		t.Fatalf("err = %v, want ErrIgnored", err)
	}
}

func TestResolverRejectsUnsupportedObjects(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	service := cluster.add("Service", testNamespace, testDeployment, "s-1", nil)
	if _, err := cluster.resolver().Resolve(context.Background(), service.Reference); !errors.Is(err, ErrNoSubject) {
		t.Fatalf("err = %v, want ErrNoSubject", err)
	}
}

func TestResolverResolvesClaimsToAConsumer(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	deployment := cluster.add(testKindDeployment, testNamespace, testDeployment, testDeploymentUID, nil)
	replicaSet := cluster.add("ReplicaSet", testNamespace, "checkout-7d9f", "rs-1", ownerRef(deployment))
	pods := []*Object{
		cluster.add(testKindPod, testNamespace, "checkout-7d9f-abc12", testPodUID, ownerRef(replicaSet)),
		cluster.add(testKindPod, testNamespace, "checkout-7d9f-def34", "p-2", ownerRef(replicaSet)),
	}
	claim := cluster.add("PersistentVolumeClaim", testNamespace, "data", "pvc-1", nil)
	references := make([]Reference, 0, len(pods))
	for _, pod := range pods {
		references = append(references, pod.Reference)
	}
	cluster.consumers[claim.Reference] = references

	subject, err := cluster.resolver().Resolve(context.Background(), claim.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if subject.Kind != testKindDeployment || subject.UID != testDeploymentUID {
		t.Fatalf("subject = %s/%s, want the single consuming Deployment", subject.Kind, subject.UID)
	}
	if subject.ResolvedFrom != "pvc" {
		t.Fatalf("resolvedFrom = %q, want pvc", subject.ResolvedFrom)
	}
}

func TestResolverKeepsSharedClaimsAsSubjects(t *testing.T) {
	t.Parallel()
	cases := map[string]int{"no consumers": 0, "two workloads": 2}
	for name, consumers := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cluster := newFakeCluster()
			claim := cluster.add("PersistentVolumeClaim", testNamespace, "data", "pvc-1", nil)
			var references []Reference
			for index := range consumers {
				deployment := cluster.add(testKindDeployment, testNamespace, "consumer-"+string(rune('a'+index)),
					"d-"+string(rune('a'+index)), nil)
				references = append(references, deployment.Reference)
			}
			cluster.consumers[claim.Reference] = references
			subject, err := cluster.resolver().Resolve(context.Background(), claim.Reference)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if subject.Kind != string(v1alpha1.TargetKindPersistentVolumeClaim) {
				t.Fatalf("subject = %s, want the claim", subject.Kind)
			}
			if len(subject.Related) != consumers {
				t.Fatalf("related = %d, want %d", len(subject.Related), consumers)
			}
		})
	}
}

func TestResolverFailsClosedWhenTheOwnerIsMissing(t *testing.T) {
	t.Parallel()
	cluster := newFakeCluster()
	missing := Reference{Kind: testKindDeployment, Namespace: testNamespace, Name: "gone", UID: "d-gone"}
	pod := cluster.add(testKindPod, testNamespace, "orphan", testPodUID, &missing)
	subject, err := cluster.resolver().Resolve(context.Background(), pod.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if subject.Resolution != v1alpha1.ResolutionPartial || subject.Kind != testKindPod {
		t.Fatalf("subject = %s (%s), want the orphaned Pod as Partial", subject.Kind, subject.Resolution)
	}
}

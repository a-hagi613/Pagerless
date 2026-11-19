package subject

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Fixtures shared by the resolver tests.
const (
	testDeployment = "checkout"
	testNamespace  = "shop"
	testAppVersion = "apps/v1"
	testQueue      = "queue"
)

// newScheme returns the scheme the resolver tests read through.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestResolveAppliesTheDocumentedOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
		rules       []Rule
		want        Subject
		resolution  Resolution
		ignore      string
	}{
		{
			name: "explicit kind and name win over a workload label",
			labels: map[string]string{
				LabelWorkloadKind: "Deployment", LabelWorkloadName: testDeployment,
				LabelNamespace: testNamespace, LabelDeployment: "ignored",
			},
			want: Subject{
				APIVersion: testAppVersion, Kind: v1alpha1.TargetKindDeployment,
				Namespace: testNamespace, Name: testDeployment,
			},
			resolution: ResolutionExplicit,
		},
		{
			name:   "one workload label resolves",
			labels: map[string]string{LabelStatefulSet: testQueue, LabelNamespace: testNamespace},
			want: Subject{
				APIVersion: testAppVersion, Kind: v1alpha1.TargetKindStatefulSet,
				Namespace: testNamespace, Name: testQueue,
			},
			resolution: ResolutionWorkloadLabel,
		},
		{
			name: "two workload labels are ambiguous and never guessed",
			labels: map[string]string{
				LabelDeployment: testDeployment, LabelStatefulSet: testQueue, LabelNamespace: testNamespace,
			},
			ignore: "ambiguous-workload-labels",
		},
		{
			name:   "a workload label without a namespace is ignored",
			labels: map[string]string{LabelDeployment: testDeployment},
			ignore: "namespace-missing",
		},
		{
			name: "a structured annotation resolves",
			annotations: map[string]string{
				AnnotationSubject: `{"kind":"DaemonSet","namespace":"kube-system","name":"agent"}`,
			},
			want: Subject{
				APIVersion: testAppVersion, Kind: v1alpha1.TargetKindDaemonSet,
				Namespace: "kube-system", Name: "agent",
			},
			resolution: ResolutionAnnotation,
		},
		{
			name:        "a malformed annotation is ignored",
			annotations: map[string]string{AnnotationSubject: "{"},
			ignore:      "malformed-subject-annotation",
		},
		{
			name:   "an extraction rule resolves a sender with no Kubernetes labels",
			labels: map[string]string{"AlarmName": "checkout-5xx-shop"},
			rules: []Rule{{
				Label: "AlarmName", Pattern: `^(?P<name>[a-z0-9-]+?)-\d+xx-(?P<namespace>[a-z0-9-]+)$`,
				Kind: v1alpha1.TargetKindDeployment,
			}},
			want: Subject{
				APIVersion: testAppVersion, Kind: v1alpha1.TargetKindDeployment,
				Namespace: testNamespace, Name: testDeployment,
			},
			resolution: ResolutionExtraction,
		},
		{
			name:       "a Node subject carries no namespace",
			labels:     map[string]string{LabelWorkloadKind: "Node", LabelWorkloadName: "ip-10-0-0-1"},
			want:       Subject{APIVersion: "v1", Kind: v1alpha1.TargetKindNode, Name: "ip-10-0-0-1"},
			resolution: ResolutionExplicit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolver := &Resolver{Rules: test.rules}
			if err := resolver.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			got, err := resolver.Resolve(context.Background(), test.labels, test.annotations)
			if test.ignore != "" {
				reason, ok := IgnoreReason(err)
				if !ok || reason != test.ignore {
					t.Fatalf("Resolve error = %v, want ignore reason %q", err, test.ignore)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Subject != test.want {
				t.Errorf("Subject = %+v, want %+v", got.Subject, test.want)
			}
			if got.Resolution != test.resolution {
				t.Errorf("Resolution = %q, want %q", got.Resolution, test.resolution)
			}
		})
	}
}

func TestResolveFollowsThePodOwnerChain(t *testing.T) {
	t.Parallel()
	const namespace = testNamespace
	deployment := &appsv1.Deployment{
		Name: testDeployment, Namespace: namespace, UID: types.UID("deployment-uid")}
	replicaSet := &appsv1.ReplicaSet{
		Name: "checkout-7c9", Namespace: namespace, UID: types.UID("replicaset-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: testAppVersion, Kind: "Deployment", Name: deployment.Name,
			UID: deployment.UID, Controller: new(true),
		}}}
	pod := &corev1.Pod{
		Name: "checkout-7c9-abc", Namespace: namespace, UID: types.UID("pod-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: testAppVersion, Kind: "ReplicaSet", Name: replicaSet.Name,
			UID: replicaSet.UID, Controller: new(true),
		}}}
	reader := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(deployment, replicaSet, pod).Build()
	resolver := &Resolver{Reader: reader}

	got, err := resolver.Resolve(context.Background(),
		map[string]string{LabelPod: pod.Name, LabelNamespace: namespace}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Subject{
		APIVersion: testAppVersion, Kind: v1alpha1.TargetKindDeployment, Namespace: namespace,
		Name: deployment.Name, UID: string(deployment.UID),
	}
	if got.Subject != want {
		t.Errorf("Subject = %+v, want %+v", got.Subject, want)
	}
	if got.Resolution != ResolutionPodOwner {
		t.Errorf("Resolution = %q, want %q", got.Resolution, ResolutionPodOwner)
	}
}

func TestResolveStopsAtAJobOwnedByACronJob(t *testing.T) {
	t.Parallel()
	const namespace = "batch"
	cronJob := &batchv1.CronJob{
		Name: "nightly", Namespace: namespace, UID: types.UID("cronjob-uid")}
	job := &batchv1.Job{
		Name: "nightly-123", Namespace: namespace, UID: types.UID("job-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob", Name: cronJob.Name, UID: cronJob.UID, Controller: new(true),
		}}}
	pod := &corev1.Pod{
		Name: "nightly-123-abc", Namespace: namespace, UID: types.UID("pod-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true),
		}}}
	reader := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cronJob, job, pod).Build()
	resolver := &Resolver{Reader: reader}

	got, err := resolver.Resolve(context.Background(),
		map[string]string{LabelPod: pod.Name, LabelNamespace: namespace}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := v1alpha1.TargetKindCronJob
	if got.Subject.Kind != want {
		t.Errorf("Kind = %q, want %q", got.Subject.Kind, want)
	}
}

func TestResolveReportsAMissingPodRatherThanGuessing(t *testing.T) {
	t.Parallel()
	resolver := &Resolver{Reader: fake.NewClientBuilder().WithScheme(newScheme(t)).Build()}
	_, err := resolver.Resolve(context.Background(),
		map[string]string{LabelPod: "gone", LabelNamespace: testNamespace}, nil)
	reason, ok := IgnoreReason(err)
	if !ok || reason != "pod-not-found" {
		t.Fatalf("Resolve error = %v, want pod-not-found", err)
	}
}

func TestCheckerReportsAMissingTargetWithoutAnError(t *testing.T) {
	t.Parallel()
	reader := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(&appsv1.Deployment{
		Name: testDeployment, Namespace: testNamespace, UID: types.UID("uid-1"),
	}).Build()
	checker := &Checker{Reader: reader}

	found, ok, err := checker.Lookup(context.Background(), Subject{
		Kind: v1alpha1.TargetKindDeployment, Namespace: testNamespace, Name: testDeployment,
	})
	if err != nil || !ok || found.UID != "uid-1" {
		t.Fatalf("Lookup existing = %+v, %v, %v", found, ok, err)
	}
	missing, ok, err := checker.Lookup(context.Background(), Subject{
		Kind: v1alpha1.TargetKindDeployment, Namespace: testNamespace, Name: "gone",
	})
	if err != nil || ok {
		t.Fatalf("Lookup missing = %+v, %v, %v", missing, ok, err)
	}
}

func TestRuleCompileRejectsAnUnusablePattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rule Rule
	}{
		{"no name group", Rule{Label: "x", Pattern: `^(?P<namespace>\w+)$`, Kind: v1alpha1.TargetKindDeployment}},
		{"no namespace", Rule{Label: "x", Pattern: `^(?P<name>\w+)$`, Kind: v1alpha1.TargetKindDeployment}},
		{"bad regex", Rule{Label: "x", Pattern: `(`, Kind: v1alpha1.TargetKindDeployment, Namespace: testNamespace}},
		{"unknown kind", Rule{Label: "x", Pattern: `(?P<name>\w+)`, Kind: "Widget", Namespace: testNamespace}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.rule.Compile(); err == nil {
				t.Fatal("Compile succeeded, want an error")
			}
		})
	}
}

// ptr returns a pointer to value.
// ensure the fake client satisfies the interface the resolver uses.
var _ client.Reader = fake.NewClientBuilder().Build()

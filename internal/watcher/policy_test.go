package watcher

import (
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func TestPolicyNamespaces(t *testing.T) {
	t.Parallel()
	policy := Policy{ReleaseNamespace: testReleaseNamespace}
	cases := map[string]struct {
		subject  Subject
		wantMode NamespaceMode
	}{
		"ordinary namespace": {
			subject:  Subject{Kind: testKindDeployment, Namespace: testNamespace},
			wantMode: ModeWatch,
		},
		"release namespace": {
			subject:  Subject{Kind: testKindDeployment, Namespace: testReleaseNamespace},
			wantMode: ModeIgnore,
		},
		"kube-public": {
			subject:  Subject{Kind: testKindPod, Namespace: "kube-public"},
			wantMode: ModeIgnore,
		},
		"kube-node-lease": {
			subject:  Subject{Kind: testKindPod, Namespace: "kube-node-lease"},
			wantMode: ModeIgnore,
		},
		"kube-system is warn-only by default": {
			subject:  Subject{Kind: testKindPod, Namespace: kubeSystemNamespace},
			wantMode: ModeWarnOnly,
		},
		"node is watched by default": {
			subject:  Subject{Kind: testKindNode, Name: testNodeName},
			wantMode: ModeWatch,
		},
	}
	for name, testCase := range cases {
		if got := policy.Evaluate(testCase.subject); got.Mode != testCase.wantMode {
			t.Errorf("%s: mode = %s, want %s", name, got.Mode, testCase.wantMode)
		}
	}
}

func TestPolicyModesAreConfigurable(t *testing.T) {
	t.Parallel()
	policy := Policy{ReleaseNamespace: testReleaseNamespace, KubeSystem: ModeWatch, NodePages: ModeIgnore}
	if got := policy.Evaluate(Subject{Kind: testKindPod, Namespace: kubeSystemNamespace}); got.Mode != ModeWatch {
		t.Errorf("kube-system mode = %s, want watch", got.Mode)
	}
	if got := policy.Evaluate(Subject{Kind: testKindNode, Name: testNodeName}); got.Mode != ModeIgnore {
		t.Errorf("node mode = %s, want ignore", got.Mode)
	}
}

func TestPolicyIncludeAndExclude(t *testing.T) {
	t.Parallel()
	included := Policy{ReleaseNamespace: testReleaseNamespace, Include: []string{testNamespace}}
	if got := included.Evaluate(Subject{Kind: testKindDeployment, Namespace: testNamespace}); got.Mode != ModeWatch {
		t.Errorf("included namespace mode = %s, want watch", got.Mode)
	}
	if got := included.Evaluate(Subject{Kind: testKindDeployment, Namespace: testOtherNamespace}); got.Mode != ModeIgnore {
		t.Errorf("namespace outside the include list mode = %s, want ignore", got.Mode)
	}
	excluded := Policy{ReleaseNamespace: testReleaseNamespace, Exclude: []string{"batch"}}
	if got := excluded.Evaluate(Subject{Kind: testKindDeployment, Namespace: "batch"}); got.Mode != ModeIgnore {
		t.Errorf("excluded namespace mode = %s, want ignore", got.Mode)
	}
}

func TestPolicyIgnoreAnnotationAndSelfExclusion(t *testing.T) {
	t.Parallel()
	policy := Policy{ReleaseNamespace: testReleaseNamespace}
	annotated := Subject{
		Kind: testKindDeployment, Namespace: testNamespace,
		Annotations: map[string]string{v1alpha1.AnnotationIgnore: "true"},
	}
	if got := policy.Evaluate(annotated); got.Mode != ModeIgnore || got.Reason != "annotation" {
		t.Errorf("ignore annotation mode = %s", got.Mode)
	}
	partOf := Subject{
		Kind: testKindDeployment, Namespace: testNamespace,
		Labels: map[string]string{v1alpha1.LabelPartOf: testReleaseNamespace},
	}
	if got := policy.Evaluate(partOf); got.Mode != ModeIgnore {
		t.Errorf("self-exclusion label mode = %s, want ignore", got.Mode)
	}
	wrongValue := Subject{
		Kind: testKindDeployment, Namespace: testNamespace,
		Labels: map[string]string{v1alpha1.LabelPartOf: testNamespace},
	}
	if got := policy.Evaluate(wrongValue); got.Mode != ModeWatch {
		t.Errorf("unrelated part-of label mode = %s, want watch", got.Mode)
	}
}

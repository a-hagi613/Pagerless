package executor_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/executor"
	"github.com/a-hagi613/pagerless/internal/executorapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

const (
	testNamespace = "pagerless"
	testWorkload  = "checkout"
	testApprover  = "alice"
)

func TestNewAllowsLiveOnlyModeAndRejectsIncompleteSandboxConfiguration(t *testing.T) {
	t.Parallel()
	scheme, err := executor.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	rw := fake.NewClientBuilder().WithScheme(scheme).Build()
	token := func() (string, error) { return "token", nil }
	validSandbox := sandbox.Repository{URL: "https://github.com/octocat/pagerless-sandbox.git", Branch: "main"}
	validGitHub := &executor.GitHub{Repository: "octocat/pagerless-sandbox", Token: token}

	cases := []struct {
		name    string
		cfg     executor.Config
		wantErr bool
	}{
		{
			name: "live-only mode without Sandbox or GitHub",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
			},
			wantErr: false,
		},
		{
			name: "full pull-request and live mode",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
				Sandbox:   validSandbox,
				GitHub:    validGitHub,
				Token:     token,
			},
			wantErr: false,
		},
		{
			name: "missing reader",
			cfg: executor.Config{
				Writer:    rw,
				Namespace: testNamespace,
			},
			wantErr: true,
		},
		{
			name: "missing writer",
			cfg: executor.Config{
				Reader:    rw,
				Namespace: testNamespace,
			},
			wantErr: true,
		},
		{
			name: "missing namespace",
			cfg: executor.Config{
				Reader: rw,
				Writer: rw,
			},
			wantErr: true,
		},
		{
			name: "Sandbox URL without GitHub",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
				Sandbox:   validSandbox,
			},
			wantErr: true,
		},
		{
			name: "GitHub without Sandbox URL",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
				GitHub:    validGitHub,
				Token:     token,
			},
			wantErr: true,
		},
		{
			name: "GitHub without top-level token",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
				Sandbox:   validSandbox,
				GitHub:    validGitHub,
			},
			wantErr: true,
		},
		{
			name: "invalid GitHub repository name",
			cfg: executor.Config{
				Reader:    rw,
				Writer:    rw,
				Namespace: testNamespace,
				Sandbox:   validSandbox,
				GitHub:    &executor.GitHub{Repository: "not-owner-slash-name", Token: token},
				Token:     token,
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := executor.New(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// replacingWriter deletes a pod and immediately creates a ready replacement pod
// in the backing fake client so RestartWorkload's rollout check completes.
type replacingWriter struct {
	client.Client
}

func (w replacingWriter) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	pod, ok := obj.(*corev1.Pod)
	if err := w.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if !ok {
		return nil
	}
	replacement := &corev1.Pod{
		Namespace: pod.Namespace,
		Name:      pod.Name + "-replacement",
		UID:       types.UID(pod.Name + "-replacement-uid"),
		Labels:    pod.Labels,
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	return w.Create(ctx, replacement)
}

func TestExecuteInLiveOnlyMode(t *testing.T) {
	t.Parallel()
	scheme, err := executor.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := metav1.NewTime(time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC))
	workloadRef := v1alpha1.WorkloadReference{
		Kind:      v1alpha1.WorkloadKindDeployment,
		Namespace: "sandbox",
		Name:      testWorkload,
	}

	t.Run("RestartWorkload executes live without a Sandbox", func(t *testing.T) {
		t.Parallel()
		deployment := &appsv1.Deployment{
			Namespace:  workloadRef.Namespace,
			Name:       workloadRef.Name,
			Generation: 2,
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": testWorkload}},
			},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 2,
				Replicas:           1,
				UpdatedReplicas:    1,
				AvailableReplicas:  1,
			},
		}
		pod := &corev1.Pod{
			Namespace: workloadRef.Namespace,
			Name:      "checkout-0",
			UID:       "checkout-0-uid",
			Labels:    map[string]string{"app": testWorkload},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		}
		tr := &v1alpha1.Triage{
			Namespace: testNamespace,
			Name:      "triage-restart",
			UID:       "triage-restart-uid",
			Spec:      v1alpha1.TriageSpec{Workload: workloadRef},
			Status: v1alpha1.TriageStatus{
				Phase: v1alpha1.PhaseApproved,
				Decision: &v1alpha1.Decision{
					Outcome:   v1alpha1.DecisionApproved,
					Approver:  testApprover,
					DecidedAt: decidedAt,
				},
			},
		}
		action := v1alpha1.ProposedAction{Action: v1alpha1.ActionRestartWorkload}
		base := &v1alpha1.ProposalBase{WorkloadGeneration: 2}
		hash, err := proposal.Hash(tr, action, nil, base)
		if err != nil {
			t.Fatal(err)
		}
		tr.Status.Proposal = &v1alpha1.Proposal{
			ProposedAction: action,
			Base:           base,
			Hash:           hash,
		}
		approval := &v1alpha1.TriageApproval{
			Namespace: testNamespace,
			Name:      v1alpha1.TriageApprovalName(tr, hash),
			Spec: v1alpha1.TriageApprovalSpec{
				Triage:       tr.Name,
				ProposalHash: hash,
				Decision:     v1alpha1.DecisionApproved,
				Approver:     testApprover,
			},
		}

		backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, pod, tr, approval).Build()
		runner, err := executor.New(executor.Config{
			Reader:         backing,
			Writer:         replacingWriter{Client: backing},
			Namespace:      testNamespace,
			RestartTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}

		resp, err := runner.Execute(t.Context(), tr.Name, hash)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if !resp.Live || !slices.Equal(resp.RestartedPods, []string{"checkout-0"}) {
			t.Errorf("Execute() = %+v, want Live with RestartedPods [checkout-0]", resp)
		}
	})

	t.Run("a pull-request Action is refused with 412 Precondition Failed when --sandbox-url is unset", func(t *testing.T) {
		t.Parallel()
		tr := &v1alpha1.Triage{
			Namespace: testNamespace,
			Name:      "triage-scale",
			UID:       "triage-scale-uid",
			Spec:      v1alpha1.TriageSpec{Workload: workloadRef},
			Status: v1alpha1.TriageStatus{
				Phase: v1alpha1.PhaseApproved,
				Decision: &v1alpha1.Decision{
					Outcome:   v1alpha1.DecisionApproved,
					Approver:  testApprover,
					DecidedAt: decidedAt,
				},
			},
		}
		action := v1alpha1.ProposedAction{
			Action:     v1alpha1.ActionScale,
			Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: 3}},
		}
		change := &v1alpha1.RenderedChange{
			Path: "apps/checkout/deployment.yaml",
			Patch: "--- a/apps/checkout/deployment.yaml\n" +
				"+++ b/apps/checkout/deployment.yaml\n" +
				"@@ -1 +1 @@\n-replicas: 1\n+replicas: 3\n",
		}
		base := &v1alpha1.ProposalBase{
			Commit:             strings.Repeat("a", 40),
			WorkloadGeneration: 1,
		}
		hash, err := proposal.Hash(tr, action, change, base)
		if err != nil {
			t.Fatal(err)
		}
		tr.Status.Proposal = &v1alpha1.Proposal{
			ProposedAction: action,
			RenderedChange: change,
			Base:           base,
			Hash:           hash,
		}
		approval := &v1alpha1.TriageApproval{
			Namespace: testNamespace,
			Name:      v1alpha1.TriageApprovalName(tr, hash),
			Spec: v1alpha1.TriageApprovalSpec{
				Triage:       tr.Name,
				ProposalHash: hash,
				Decision:     v1alpha1.DecisionApproved,
				Approver:     testApprover,
			},
		}

		backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tr, approval).Build()
		runner, err := executor.New(executor.Config{
			Reader:    backing,
			Writer:    backing,
			Namespace: testNamespace,
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = runner.Execute(t.Context(), tr.Name, hash)
		var refusal *executorapi.Error
		if !errors.As(err, &refusal) {
			t.Fatalf("Execute() error = %v, want *executorapi.Error", err)
		}
		if refusal.StatusCode != http.StatusPreconditionFailed ||
			refusal.Reason != executorapi.ReasonUnsupportedAction ||
			!strings.Contains(refusal.Message, "without --sandbox-url") {
			t.Errorf("refusal = %+v, want 412 UnsupportedAction mentioning --sandbox-url", refusal)
		}
	})
}

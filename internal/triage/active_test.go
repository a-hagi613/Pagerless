package triage_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/triage"
)

func TestCanonicalActiveTriagePreservesApprovedWorkAndOrdersUnapprovedTies(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewTime(time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC))
	workload := v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "checkout"}
	makeTriage := func(name string, phase v1alpha1.Phase) *v1alpha1.Triage {
		return &v1alpha1.Triage{
			Name: name, Namespace: "pagerless", UID: types.UID(name), CreationTimestamp: at,
			Spec: v1alpha1.TriageSpec{Workload: workload}, Status: v1alpha1.TriageStatus{Phase: phase},
		}
	}
	t.Run("an existing execution wins an equal-timestamp late name", func(t *testing.T) {
		executing := makeTriage("z-executing", v1alpha1.PhaseExecuting)
		late := makeTriage("a-late", v1alpha1.PhasePending)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(executing, late).Build()
		winner, losers, err := triage.CanonicalActive(t.Context(), reader, "pagerless", workload)
		if err != nil || winner == nil || winner.Name != executing.Name || len(losers) != 1 || losers[0].Name != late.Name {
			t.Fatalf("winner = %+v, losers = %+v, error = %v, want existing execution", winner, losers, err)
		}
	})
	t.Run("unapproved ties use a stable name order", func(t *testing.T) {
		first, second := makeTriage("a-first", v1alpha1.PhasePending), makeTriage("z-second", v1alpha1.PhasePending)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(second, first).Build()
		winner, _, err := triage.CanonicalActive(t.Context(), reader, "pagerless", workload)
		if err != nil || winner == nil || winner.Name != first.Name {
			t.Fatalf("winner = %+v, error = %v, want the canonical name for an unapproved tie", winner, err)
		}
	})
	t.Run("multiple approved legacy Triages fail closed", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			makeTriage("first", v1alpha1.PhaseExecuting), makeTriage("second", v1alpha1.PhaseApproved),
		).Build()
		_, _, err := triage.CanonicalActive(t.Context(), reader, "pagerless", workload)
		if !errors.Is(err, triage.ErrConflictingApprovals) {
			t.Fatalf("error = %v, want conflicting Approvals preserved without selecting a loser", err)
		}
	})
	t.Run("an admitted Approval is protected before its status is applied", func(t *testing.T) {
		proposed := makeTriage("z-approved", v1alpha1.PhaseProposed)
		hash := strings.Repeat("a", 64)
		proposed.Status.Proposal = &v1alpha1.Proposal{Hash: hash}
		approval := &v1alpha1.TriageApproval{
			Namespace: proposed.Namespace, Name: v1alpha1.TriageApprovalName(proposed, hash),
			Spec: v1alpha1.TriageApprovalSpec{
				Triage: proposed.Name, ProposalHash: hash, Decision: v1alpha1.DecisionApproved, Approver: "alice",
			},
		}
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			proposed, approval, makeTriage("a-late", v1alpha1.PhasePending),
		).Build()
		winner, _, err := triage.CanonicalActive(t.Context(), reader, "pagerless", workload)
		if err != nil || winner == nil || winner.Name != proposed.Name {
			t.Fatalf("winner = %+v, error = %v, want the admitted human Approval preserved", winner, err)
		}
		legacy := approval.DeepCopy()
		legacy.Name = "legacy-generated-decision"
		reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			proposed, legacy, makeTriage("a-late", v1alpha1.PhasePending),
		).Build()
		winner, _, err = triage.CanonicalActive(t.Context(), reader, "pagerless", workload)
		if err != nil || winner == nil || winner.Name != proposed.Name {
			t.Fatalf("winner = %+v, error = %v, want the admitted legacy Approval preserved", winner, err)
		}
	})
}

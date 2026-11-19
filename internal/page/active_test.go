package page_test

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
	"github.com/a-hagi613/pagerless/internal/page"
)

func TestCanonicalActivePageSelectsOnePagePerFingerprint(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewTime(time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC))
	target := v1alpha1.TargetReference{Kind: v1alpha1.TargetKindDeployment, Namespace: "sandbox", Name: "checkout"}
	fingerprint := v1alpha1.Fingerprint(target, "CrashLoopBackOff")
	makePage := func(name, fingerprint string, phase v1alpha1.Phase) *v1alpha1.Page {
		return &v1alpha1.Page{
			Name: name, Namespace: "pagerless", UID: types.UID(name), CreationTimestamp: at,
			Spec: v1alpha1.PageSpec{
				Target: target, Fingerprint: fingerprint,
				Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent},
			},
			Status: v1alpha1.PageStatus{Phase: phase},
		}
	}
	t.Run("approved work wins an equal-timestamp late name", func(t *testing.T) {
		approved := makePage("z-approved", fingerprint, v1alpha1.PhasePullRequestOpen)
		late := makePage("a-late", fingerprint, v1alpha1.PhasePending)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(approved, late).Build()
		winner, losers, err := page.CanonicalActive(t.Context(), reader, "pagerless", fingerprint)
		if err != nil || winner == nil || winner.Name != approved.Name || len(losers) != 1 ||
			losers[0].Name != late.Name {
			t.Fatalf("winner = %+v, losers = %+v, error = %v, want the approved Page", winner, losers, err)
		}
	})
	t.Run("unapproved ties use a stable name order", func(t *testing.T) {
		first, second := makePage("a-first", fingerprint, v1alpha1.PhasePending),
			makePage("z-second", fingerprint, v1alpha1.PhasePending)
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(second, first).Build()
		winner, _, err := page.CanonicalActive(t.Context(), reader, "pagerless", fingerprint)
		if err != nil || winner == nil || winner.Name != first.Name {
			t.Fatalf("winner = %+v, error = %v, want the canonical name for an unapproved tie", winner, err)
		}
	})
	t.Run("a different Fingerprint is a different Page", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			makePage("crashloop", fingerprint, v1alpha1.PhaseInvestigating),
			makePage("unschedulable", strings.Repeat("b", 64), v1alpha1.PhaseInvestigating),
		).Build()
		winner, losers, err := page.CanonicalActive(t.Context(), reader, "pagerless", fingerprint)
		if err != nil || winner == nil || winner.Name != "crashloop" || len(losers) != 0 {
			t.Fatalf("winner = %+v, losers = %+v, error = %v, want only the matching Fingerprint", winner, losers, err)
		}
	})
	t.Run("a terminal Page is not canonical", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			makePage("finished", fingerprint, v1alpha1.PhaseRecovered),
		).Build()
		winner, _, err := page.CanonicalActive(t.Context(), reader, "pagerless", fingerprint)
		if err != nil || winner != nil {
			t.Fatalf("winner = %+v, error = %v, want no canonical Page", winner, err)
		}
	})
	t.Run("multiple Pages with approved work fail closed", func(t *testing.T) {
		reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			makePage("first", fingerprint, v1alpha1.PhasePullRequestOpen),
			makePage("second", fingerprint, v1alpha1.PhaseApproved),
		).Build()
		_, _, err := page.CanonicalActive(t.Context(), reader, "pagerless", fingerprint)
		if !errors.Is(err, page.ErrConflictingApprovals) {
			t.Fatalf("error = %v, want a conflict rather than an abandoned pull request", err)
		}
	})
}

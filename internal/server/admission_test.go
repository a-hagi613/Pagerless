package server_test

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

func TestSecondActiveTriageForTheSameWorkloadIsRefused(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	first := env.CreateTriage(t, testenv.NewTriage(checkout))

	err := env.Client.Create(t.Context(), testenv.NewTriage(checkout))
	if err == nil {
		t.Fatal("a second Active Triage for the same Workload was admitted")
	}
	if !apierrors.IsAlreadyExists(err) {
		t.Errorf("error = %v, want AlreadyExists", err)
	}
	if !strings.Contains(err.Error(), first.Name) {
		t.Errorf("error %q does not name the existing Triage %q", err, first.Name)
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil || status.Status().Details.Name != first.Name {
		t.Errorf("error details = %+v, want the existing Triage's name %q", status, first.Name)
	}

	t.Run("another Workload is admitted", func(t *testing.T) {
		env.CreateTriage(t, testenv.NewTriage("cart"))
	})
	t.Run("the same name in another namespace is another Workload", func(t *testing.T) {
		other := testenv.NewTriage(checkout)
		other.Spec.Workload.Namespace = "staging"
		env.CreateTriage(t, other)
	})
	t.Run("a terminal Triage no longer blocks the Workload", func(t *testing.T) {
		env.WaitForPhase(t, first.Name, v1alpha1.PhaseInvestigating)
		env.EndJob(t, env.Get(t, first.Name).Status.JobName, testenv.JobFailed)
		env.WaitForPhase(t, first.Name, v1alpha1.PhaseFailed)
		env.CreateTriage(t, testenv.NewTriage(checkout))
	})
}

func TestTriageOutsideTheServerNamespaceIsRefused(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	tr := testenv.NewTriage(checkout)
	tr.Namespace = metav1.NamespaceDefault

	err := env.Client.Create(t.Context(), tr)
	if err == nil || !strings.Contains(err.Error(), testenv.Namespace) {
		t.Fatalf("error = %v, want a refusal that names namespace %q", err, testenv.Namespace)
	}
}

func TestOnlyPagerlessServerWritesTriageStatus(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	// Even a cluster administrator cannot write a Proposal, a phase, or a Diagnosis.
	tr.Status.Phase = v1alpha1.PhaseProposed
	tr.Status.Diagnosis = "forged"
	err := env.Client.Status().Update(t.Context(), tr)
	if err == nil || !strings.Contains(err.Error(), "only pagerless-server writes Triage status") {
		t.Fatalf("status update by an administrator: error = %v, want the status guard's refusal", err)
	}
	if got := env.Get(t, created.Name).Status; got.Phase != v1alpha1.PhaseInvestigating || got.Diagnosis != "" {
		t.Errorf("status after the refused update = %+v", got)
	}
}

func TestTriageSpecIsImmutable(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	// Let the server finish writing status, so that an update conflict cannot mask the immutability error.
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	tests := map[string]func(*v1alpha1.Triage){
		"hint":      func(tr *v1alpha1.Triage) { tr.Spec.Hint = "look at the database" },
		"workload":  func(tr *v1alpha1.Triage) { tr.Spec.Workload.Name = "cart" },
		"trigger":   func(tr *v1alpha1.Triage) { tr.Spec.Trigger.Source = v1alpha1.TriggerSourceDashboard },
		"followsTo": func(tr *v1alpha1.Triage) { tr.Spec.FollowsTriage = "triage-earlier" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tr := env.Get(t, created.Name)
			mutate(tr)
			err := env.Client.Update(t.Context(), tr)
			if err == nil || !strings.Contains(err.Error(), "spec is immutable") {
				t.Errorf("error = %v, want %q", err, "spec is immutable")
			}
		})
	}
}

func TestTriageResourceIsUsableFromKubectl(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	t.Run("short name trg", func(t *testing.T) {
		resources, err := env.Clientset.Discovery().ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range resources.APIResources {
			if r.Name == triagesResource {
				if len(r.ShortNames) != 1 || r.ShortNames[0] != "trg" {
					t.Errorf("short names = %v, want [trg]", r.ShortNames)
				}
				return
			}
		}
		t.Error("the triages resource is not served")
	})

	t.Run("printer columns", func(t *testing.T) {
		table := env.Table(t, triagesResource)
		names := make([]string, 0, len(table.ColumnDefinitions))
		for _, c := range table.ColumnDefinitions {
			names = append(names, c.Name)
		}
		want := []string{"Name", "Phase", "Namespace", "Workload", "Action", "Triggers", "Age"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("columns = %v, want %v", names, want)
		}
		if len(table.Rows) != 1 || table.Rows[0].Cells[1] != "Investigating" || table.Rows[0].Cells[3] != checkout {
			t.Errorf("rows = %+v, want one Investigating Triage for checkout", table.Rows)
		}
	})
}

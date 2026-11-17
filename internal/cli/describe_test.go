package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// The server writes Execution and Verification only after a human decision,
// so the presentation of those sections is checked on a fully populated Triage.
func TestDescribeWritesEverySection(t *testing.T) {
	t.Parallel()
	decided := metav1.NewTime(time.Date(2026, time.November, 16, 12, 30, 0, 0, time.UTC))
	tr := &v1alpha1.Triage{
		Name:              "checkout-x2x4q",
		CreationTimestamp: decided,
		Spec: v1alpha1.TriageSpec{
			Workload: v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "checkout"},
			Hint:     "started after the memory change",
			Trigger:  v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI, Pod: "checkout-7d9f-x2x4q"},
		},
		Status: v1alpha1.TriageStatus{
			Phase:        v1alpha1.PhaseRecovered,
			TriggerCount: 1,
			Diagnosis:    "The checkout container exceeds its memory limit.\nIts limit is 128Mi.",
			Findings: []v1alpha1.Finding{{
				Kind:    v1alpha1.FindingKindLog,
				Source:  "loki: checkout",
				Summary: "killed with exit code 137",
			}},
			Proposal: &v1alpha1.Proposal{
				Action: v1alpha1.ActionPatchResources,
				Parameters: &v1alpha1.ActionParameters{PatchResources: &v1alpha1.PatchResourcesParameters{
					Container: "checkout",
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("512Mi"),
						corev1.ResourceCPU:    resource.MustParse("500m"),
					},
				}},
				RenderedChange: &v1alpha1.RenderedChange{
					Path:  "apps/checkout/deployment.yaml",
					Patch: "-          memory: 128Mi\n+          memory: 512Mi",
				},
				Base:      &v1alpha1.ProposalBase{Commit: "4f2a9c1", WorkloadGeneration: 3},
				Hash:      strings.Repeat("a", 64),
				Attempt:   1,
				ExpiresAt: decided,
			},
			Decision: &v1alpha1.Decision{
				Outcome:   v1alpha1.DecisionApproved,
				Approver:  "oncall@example.com",
				Reason:    "within the bounds",
				DecidedAt: decided,
			},
			Execution: &v1alpha1.Execution{
				PullRequestURL: "https://github.com/example/sandbox/pull/12",
				MergeCommit:    "9b1d3e7",
			},
			VerificationChecks: []v1alpha1.VerificationCheck{
				{Name: "pods ready", Passed: true, Message: "3 of 3 pods are ready"},
				{Name: "no restarts", Passed: false, Message: "2 restarts since the merge"},
			},
			Narrative: "The checkout Deployment was OOMKilled after its memory limit was lowered.\n" +
				"The approved change raised the limit, and Verification found the rollout available.",
		},
	}

	var out bytes.Buffer
	if err := describe(&out, tr); err != nil {
		t.Fatalf("describe: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Phase:      Recovered",
		"Workload:   Deployment/sandbox/checkout",
		"Trigger:    CLI (pod checkout-7d9f-x2x4q)",
		"Hint:       started after the memory change",
		"Diagnosis\n  The checkout container exceeds its memory limit.\n  Its limit is 128Mi.",
		"  1. Log: loki: checkout",
		"     killed with exit code 137",
		"  Action:     PatchResources",
		"  Parameters: container checkout, limits cpu=500m memory=512Mi",
		"  Base:       commit 4f2a9c1, Workload generation 3",
		"  Path:       apps/checkout/deployment.yaml",
		"    +          memory: 512Mi",
		"  Outcome:  Approved",
		"  Reason:   within the bounds",
		"  Pull request:   https://github.com/example/sandbox/pull/12",
		"  Merge commit:   9b1d3e7",
		"  Restarted pods: none",
		"  PASS  pods ready: 3 of 3 pods are ready",
		"  FAIL  no restarts: 2 restarts since the merge",
		"Narrative\n  The checkout Deployment was OOMKilled after its memory limit was lowered.",
		"  The approved change raised the limit, and Verification found the rollout available.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("presentation lacks %q:\n%s", want, text)
		}
	}
}

func TestDescribeMarksSectionsThatHaveNoContentAsNone(t *testing.T) {
	t.Parallel()
	tr := &v1alpha1.Triage{
		Name: "cart-b7k2m",
		Spec: v1alpha1.TriageSpec{
			Workload: v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "cart"},
			Trigger:  v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI},
		},
		Status: v1alpha1.TriageStatus{Phase: v1alpha1.PhaseInvestigating},
	}

	var out bytes.Buffer
	if err := describe(&out, tr); err != nil {
		t.Fatalf("describe: %v", err)
	}
	text := out.String()
	if strings.Contains(text, "Hint:") || strings.Contains(text, "Failure:") {
		t.Errorf("presentation shows fields that the Triage does not have:\n%s", text)
	}
	for _, section := range []string{"Diagnosis\n  none", "Findings\n  none", "Proposal\n  none",
		"Decision\n  none", "Execution\n  none", "Verification\n  none", "Narrative\n  none"} {
		if !strings.Contains(text, section) {
			t.Errorf("presentation lacks the placeholder for %q:\n%s", section, text)
		}
	}
}

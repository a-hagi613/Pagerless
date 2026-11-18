//go:build preview

// The dashboard preview test runs pagerless-server and its embedded React
// dashboard against an envtest API server and a local Keycloak container, with
// realistic Triages seeded so an operator can exercise sign-in, the single-port
// Keycloak realm proxy, Triage list and detail views, Start Triage, and
// interactive Approval or Rejection over Tailscale or localhost.
package server_test

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/dashboardauth"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

func TestDashboardPreview(t *testing.T) {
	bindAddress := previewEnvOrDefault("PAGERLESS_PREVIEW_BIND", "0.0.0.0:8080")
	baseURL := previewEnvOrDefault("PAGERLESS_PREVIEW_BASE_URL", "http://devbox:8080")
	issuer := previewRequiredEnv(t, "PAGERLESS_PREVIEW_ISSUER")
	username := previewRequiredEnv(t, "PAGERLESS_PREVIEW_APPROVER")
	secretFile := previewRequiredEnv(t, "PAGERLESS_PREVIEW_CLIENT_SECRET_FILE")
	readyFile := os.Getenv("PAGERLESS_PREVIEW_READY_FILE")

	auth, err := dashboardauth.New(dashboardauth.Config{
		Issuer:   issuer,
		ClientID: "pagerless-dashboard",
		ClientSecret: func() (string, error) {
			raw, err := os.ReadFile(secretFile)
			return strings.TrimSpace(string(raw)), err
		},
		RedirectURL:    strings.TrimSuffix(baseURL, "/") + "/auth/callback",
		ApproversGroup: "pagerless-approvers",
		SecureCookies:  false,
	})
	if err != nil {
		t.Fatalf("configure preview sign-in: %v", err)
	}

	env := testenv.Start(t,
		withPatchResources(),
		testenv.WithDashboardAuth(auth),
		testenv.WithDashboardAddress(bindAddress),
		testenv.WithApprovers(username),
	)

	recoveredName := seedRecoveredTriage(t, env, "cart")
	env.Clock.Step(time.Minute)
	proposedName := seedProposedOOMTriage(t, env, checkout)
	deploy(t, env, workload{name: "frontend"})

	if readyFile != "" {
		if err := os.WriteFile(readyFile, []byte("ready\n"), 0o600); err != nil {
			t.Fatalf("write preview ready file: %v", err)
		}
	}
	t.Logf("dashboard preview listening on %s (%s) with Proposed Triage %s and Recovered Triage %s",
		bindAddress, baseURL, proposedName, recoveredName)

	seen := map[string]bool{
		recoveredName: true,
		proposedName:  true,
	}
	sigCtx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-sigCtx.Done():
			return
		case <-ticker.C:
			driveInteractiveTriages(t, env, seen)
		}
	}
}

// seedRecoveredTriage drives a Triage on the named Workload through Proposal,
// Approval, pull request merge, Argo CD sync, Verification, and Narrative so
// the preview includes a complete Recovered record.
func seedRecoveredTriage(t *testing.T, env *testenv.Env, workloadName string) string {
	t.Helper()
	deploy(t, env, workload{name: workloadName})
	created := env.CreateTriage(t, testenv.NewTriage(workloadName))
	agent := env.StartAgent(t, created.Name)

	finding := logFinding
	for _, step := range []agentapi.StepEvent{
		{Kind: v1alpha1.StepKindProgress, Summary: "gathered the Context Bundle for Deployment sandbox/" + workloadName},
		{Kind: v1alpha1.StepKindToolCall, Tool: podsLogTool, Summary: "read previous container termination logs"},
		{
			Kind:    v1alpha1.StepKindFinding,
			Summary: "container was terminated with OOMKilled (exit code 137)",
			Finding: &finding,
		},
	} {
		if res, err := agent.API.PostEvent(t.Context(), step); err != nil || res.StatusCode != http.StatusAccepted {
			t.Fatalf("post preview step for %s: %+v, %v", workloadName, res, err)
		}
	}
	res, err := agent.API.PostReport(t.Context(), withUsage(
		patchReport("app", nil, quantities("memory", "512Mi")), reportUsage,
	))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("post preview report for %s: %+v, %v", workloadName, res, err)
	}
	proposed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	approve(t, env, proposed)
	withPullRequest(t, env, created.Name)
	commit := env.GitHub.CompleteChecks(t, 1)
	tr := env.WaitFor(t, created.Name, "the merge commit is recorded", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Execution != nil && tr.Status.Execution.MergeCommit != ""
	})
	verifying(t, env, tr, commit)
	env.Clock.Step(5 * time.Minute)
	env.WaitFor(t, created.Name, "the Triage is Recovered with a Narrative Job", func(tr *v1alpha1.Triage) bool {
		return tr.Status.Phase == v1alpha1.PhaseRecovered && tr.Status.NarrativeJobName != ""
	})
	narrative := env.StartNarrative(t, created.Name)
	narrativeText := fmt.Sprintf(
		"The %s Deployment exceeded its 256Mi memory limit and was OOMKilled. "+
			"The approved PatchResources Proposal raised the memory limit to 512Mi, "+
			"Argo CD synced the merged pull request, and Verification confirmed zero restarts across the observation window.",
		workloadName,
	)
	if nRes, nErr := narrative.API.PostNarrative(t.Context(), agentapi.NarrativeReport{
		Narrative: narrativeText,
	}); nErr != nil || nRes.StatusCode != http.StatusAccepted {
		t.Fatalf("post preview Narrative: %+v, %v", nRes, nErr)
	}
	eventually(t, "the preview Triage records its Narrative", func() bool {
		return env.Get(t, created.Name).Status.Narrative == narrativeText
	})
	return created.Name
}

// seedProposedOOMTriage creates an OOMKilled Triage on the named Workload and
// leaves it in PhaseProposed with Findings, Diagnosis, and a validated
// PatchResources Proposal and rendered diff for interactive decision testing.
func seedProposedOOMTriage(t *testing.T, env *testenv.Env, workloadName string) string {
	t.Helper()
	deploy(t, env, workload{name: workloadName})
	candidate := testenv.NewTriage(workloadName)
	candidate.Spec.Trigger = alertTrigger
	created := env.CreateTriage(t, candidate)
	agent := env.StartAgent(t, created.Name)

	finding := logFinding
	metricFinding := v1alpha1.Finding{
		Kind:    v1alpha1.FindingKindMetric,
		Source:  `prometheus: container_memory_working_set_bytes{namespace="sandbox",pod="checkout-7d9f8-x2k4q"}`,
		Summary: "working set reached 255Mi against the 256Mi container memory limit before termination",
	}
	for _, step := range []agentapi.StepEvent{
		{Kind: v1alpha1.StepKindProgress, Summary: "gathered the Context Bundle for Deployment sandbox/" + workloadName},
		{
			Kind:    v1alpha1.StepKindToolCall,
			Tool:    podsLogTool,
			Summary: "inspected terminated container logs on checkout-7d9f8-x2k4q",
		},
		{Kind: v1alpha1.StepKindFinding, Summary: "container exited with code 137 (OOMKilled)", Finding: &finding},
		{Kind: v1alpha1.StepKindFinding, Summary: "working set saturated the 256Mi memory limit", Finding: &metricFinding},
	} {
		if res, err := agent.API.PostEvent(t.Context(), step); err != nil || res.StatusCode != http.StatusAccepted {
			t.Fatalf("post preview step for %s: %+v, %v", workloadName, res, err)
		}
	}
	report := patchReport("app", nil, quantities("memory", "512Mi"))
	report.Findings = []v1alpha1.Finding{finding, metricFinding}
	res, err := agent.API.PostReport(t.Context(), withUsage(report, reportUsage))
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("post preview report for %s: %+v, %v", workloadName, res, err)
	}
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	return created.Name
}

// driveInteractiveTriages advances any new Triage started from the Start Triage
// page so interactive sessions see live SSE steps and a Proposal.
func driveInteractiveTriages(t *testing.T, env *testenv.Env, seen map[string]bool) {
	t.Helper()
	var list v1alpha1.TriageList
	if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		return
	}
	for i := range list.Items {
		tr := &list.Items[i]
		if seen[tr.Name] || tr.Status.Phase != v1alpha1.PhaseInvestigating {
			continue
		}
		seen[tr.Name] = true
		agent := env.StartAgent(t, tr.Name)
		finding := logFinding
		for _, step := range []agentapi.StepEvent{
			{Kind: v1alpha1.StepKindProgress, Summary: "gathered the Context Bundle for " + tr.Spec.Workload.Name},
			{Kind: v1alpha1.StepKindToolCall, Tool: podsLogTool, Summary: "read container logs for " + tr.Spec.Workload.Name},
			{Kind: v1alpha1.StepKindFinding, Summary: "recorded log evidence for " + tr.Spec.Workload.Name, Finding: &finding},
		} {
			_, _ = agent.API.PostEvent(t.Context(), step)
			time.Sleep(500 * time.Millisecond)
		}
		_, _ = agent.API.PostReport(t.Context(), withUsage(noActionReport(), reportUsage))
	}
}

func previewEnvOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func previewRequiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is not set; run hack/dashboard-preview.sh start", name)
	}
	return value
}

package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	checkout        = "checkout"
	pagerlessGroup  = "pagerless.dev"
	podsResource    = "pods"
	triagesResource = "triages"
)

var logFinding = v1alpha1.Finding{
	Kind:    v1alpha1.FindingKindLog,
	Source:  "loki: {namespace=\"sandbox\",app=\"checkout\"}",
	Summary: "the container was killed with exit code 137 shortly after start",
}

func noActionReport() agentapi.Report {
	return agentapi.Report{
		Diagnosis: "The checkout container exceeds its memory limit and is OOMKilled.",
		Findings:  []v1alpha1.Finding{logFinding},
		Proposal:  v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction},
	}
}

func TestNewTriageStartsAnAgentJob(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)

	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	if tr.Status.TriggerCount != 1 {
		t.Errorf("triggerCount = %d, want 1", tr.Status.TriggerCount)
	}
	job, err := env.GetJob(t.Context(), tr.Status.JobName)
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.ActiveDeadlineSeconds; got == nil || *got != 300 {
		t.Errorf("activeDeadlineSeconds = %v, want 300", got)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "pagerless-agent" {
		t.Errorf("serviceAccountName = %q, want pagerless-agent", got)
	}
	if got := job.Spec.Template.Spec.AutomountServiceAccountToken; got == nil || *got {
		t.Errorf("the agent pod mounts the default service account token, want it disabled")
	}
	var audiences []string
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				audiences = append(audiences, source.ServiceAccountToken.Audience)
			}
		}
	}
	if len(audiences) != 2 || audiences[0] != agentapi.Audience ||
		audiences[1] != "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless" {
		t.Errorf("projected token audiences = %v, want the report and Keycloak realm audiences", audiences)
	}
	if owner := job.OwnerReferences; len(owner) != 1 || owner[0].Kind != "Triage" || owner[0].Name != created.Name {
		t.Errorf("Job owner references = %+v, want the Triage", owner)
	}
	env0 := job.Spec.Template.Spec.Containers[0].Env
	want := map[string]string{
		agentapi.EnvTriage:            created.Name,
		agentapi.EnvWorkloadKind:      "Deployment",
		agentapi.EnvWorkloadNamespace: "sandbox",
		agentapi.EnvWorkloadName:      checkout,
		agentapi.EnvServerURL:         env.ReportURL,
	}
	for _, v := range env0 {
		delete(want, v.Name)
	}
	if len(want) > 0 {
		t.Errorf("the agent container lacks environment variables %v", want)
	}
}

func TestAgentServiceAccountHasNoWritePermissions(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)

	user := "system:serviceaccount:" + testenv.Namespace + ":pagerless-agent"
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + testenv.Namespace, "system:authenticated"}
	writes := []struct{ group, resource, subresource string }{
		{pagerlessGroup, triagesResource, ""},
		{pagerlessGroup, triagesResource, "status"},
		{"batch", "jobs", ""},
		{"", podsResource, ""},
		{"", podsResource, "exec"},
		{"", "secrets", ""},
		{"", "configmaps", ""},
		{"apps", "deployments", ""},
		{"rbac.authorization.k8s.io", "clusterrolebindings", ""},
	}
	for _, w := range writes {
		for _, verb := range []string{"create", updateVerb, "patch", "delete", "deletecollection"} {
			if env.Can(t, user, groups, verb, w.group, w.resource, w.subresource) {
				t.Errorf("the agent service account may %s %s/%s %s", verb, w.group, w.resource, w.subresource)
			}
		}
	}
	for _, w := range []struct{ group, resource, subresource string }{
		{"", "secrets", ""},
		{"", podsResource, "exec"},
		{"", podsResource, "log"},
		{pagerlessGroup, triagesResource, ""},
	} {
		if env.Can(t, user, groups, "get", w.group, w.resource, w.subresource) {
			t.Errorf("the agent service account may get %s/%s %s without a grant for it", w.group, w.resource, w.subresource)
		}
	}
}

func TestNoActionReportMovesTheTriageToProposed(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	agent := env.StartAgent(t, created.Name)

	result, err := agent.API.PostReport(t.Context(), noActionReport())
	if err != nil {
		t.Fatal(err)
	}
	accepted := result.Report != nil && result.Report.Accepted && result.Report.Phase == v1alpha1.PhaseProposed
	if result.StatusCode != http.StatusOK || !accepted {
		t.Fatalf("report answer = %+v, want accepted in phase Proposed", result)
	}

	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	if tr.Status.Diagnosis != noActionReport().Diagnosis {
		t.Errorf("diagnosis = %q", tr.Status.Diagnosis)
	}
	if len(tr.Status.Findings) != 1 || tr.Status.Findings[0] != logFinding {
		t.Errorf("findings = %+v, want the reported finding", tr.Status.Findings)
	}
	p := tr.Status.Proposal
	if p == nil {
		t.Fatal("status.proposal is not set")
	}
	if p.Action != v1alpha1.ActionNoAction || p.Parameters != nil || p.Attempt != 1 {
		t.Errorf("proposal = %+v, want NoAction, no parameters, attempt 1", p)
	}
	if len(p.Hash) != 64 || strings.Trim(p.Hash, "0123456789abcdef") != "" {
		t.Errorf("proposal hash = %q, want 64 lowercase hexadecimal digits", p.Hash)
	}
	if want := testenv.StartTime.Add(time.Hour); !p.ExpiresAt.Time.Equal(want) {
		t.Errorf("proposal expiresAt = %v, want %v", p.ExpiresAt.Time, want)
	}

	// An agent exits after its report, which completes its Job. That must not fail the Triage.
	env.EndJob(t, tr.Status.JobName, testenv.JobComplete)
	time.Sleep(500 * time.Millisecond)
	if got := env.Get(t, created.Name).Status.Phase; got != v1alpha1.PhaseProposed {
		t.Errorf("phase after the Job completed = %s, want Proposed", got)
	}
}

func TestProposalHashIsBoundToTheTriage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	hashes := map[string]bool{}
	for _, workload := range []string{checkout, "cart"} {
		created := env.CreateTriage(t, testenv.NewTriage(workload))
		agent := env.StartAgent(t, created.Name)
		if _, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil {
			t.Fatal(err)
		}
		hashes[env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed).Status.Proposal.Hash] = true
	}
	if len(hashes) != 2 {
		t.Errorf("two Triages produced the same hash for the same Proposal")
	}
}

func TestStepEventsAreRecordedAsABoundedSummary(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	agent := env.StartAgent(t, created.Name)

	total := v1alpha1.MaxSteps + 10
	for i := range total {
		result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
			Kind:    v1alpha1.StepKindToolCall,
			Tool:    "kubernetes.pods_log",
			Summary: fmt.Sprintf("call %d", i),
		})
		if err != nil || result.StatusCode != http.StatusAccepted {
			t.Fatalf("post event %d: status %d, error %v", i, result.StatusCode, err)
		}
	}
	long := strings.Repeat("x", 2*agentapi.MaxStepSummaryLength)
	longEvent := agentapi.StepEvent{Kind: v1alpha1.StepKindProgress, Summary: long}
	if result, err := agent.API.PostEvent(t.Context(), longEvent); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post long event: status %d, error %v", result.StatusCode, err)
	}

	tr := env.Get(t, created.Name)
	if got := len(tr.Status.Steps); got != v1alpha1.MaxSteps {
		t.Errorf("recorded %d steps, want the most recent %d", got, v1alpha1.MaxSteps)
	}
	if tr.Status.StepCount != int32(total+1) || tr.Status.ToolCallCount != int32(total) {
		t.Errorf("stepCount = %d, toolCallCount = %d, want %d and %d",
			tr.Status.StepCount, tr.Status.ToolCallCount, total+1, total)
	}
	last := tr.Status.Steps[len(tr.Status.Steps)-1]
	if got := len([]rune(last.Summary)); got != agentapi.MaxStepSummaryLength {
		t.Errorf("long summary was recorded with %d characters, want it cut to %d", got, agentapi.MaxStepSummaryLength)
	}
	if first := tr.Status.Steps[0]; first.Summary != fmt.Sprintf("call %d", total-v1alpha1.MaxSteps+1) {
		t.Errorf("oldest recorded step = %q, want the oldest steps dropped first", first.Summary)
	}
}

func TestAgentJobEndingWithoutAReportFailsTheTriageAndKeepsFindings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		end        testenv.JobEnd
		wantReason string
	}{
		{"deadline exceeded", testenv.JobDeadlineExceeded, v1alpha1.ReasonAgentDeadlineExceeded},
		{"failed", testenv.JobFailed, v1alpha1.ReasonAgentEndedWithoutReport},
		{"completed without a report", testenv.JobComplete, v1alpha1.ReasonAgentEndedWithoutReport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.Start(t)
			created := env.CreateTriage(t, testenv.NewTriage(checkout))
			agent := env.StartAgent(t, created.Name)
			result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
				Kind: v1alpha1.StepKindFinding, Summary: "recorded a log finding", Finding: &logFinding,
			})
			if err != nil || result.StatusCode != http.StatusAccepted {
				t.Fatalf("post finding event: status %d, error %v", result.StatusCode, err)
			}

			env.EndJob(t, env.Get(t, created.Name).Status.JobName, tc.end)

			tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseFailed)
			if len(tr.Status.Findings) != 1 || tr.Status.Findings[0] != logFinding {
				t.Errorf("findings = %+v, want the finding received before the failure", tr.Status.Findings)
			}
			condition := meta.FindStatusCondition(tr.Status.Conditions, v1alpha1.ConditionFailed)
			if condition == nil || condition.Status != "True" || condition.Reason != tc.wantReason {
				t.Errorf("Failed condition = %+v, want reason %s", condition, tc.wantReason)
			}

			late, err := agent.API.PostReport(t.Context(), noActionReport())
			if err != nil {
				t.Fatal(err)
			}
			if late.StatusCode != http.StatusConflict {
				t.Errorf("report after Failed: status %d, want 409", late.StatusCode)
			}
			if got := env.Get(t, created.Name).Status.Phase; got != v1alpha1.PhaseFailed {
				t.Errorf("phase after a late report = %s, want Failed", got)
			}
		})
	}
}

func TestDeletedAgentJobFailsTheTriage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	tr := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)

	job, err := env.GetJob(t.Context(), tr.Status.JobName)
	if err != nil {
		t.Fatal(err)
	}
	// Background propagation, because envtest runs no garbage collector to complete an orphaning delete.
	background := client.PropagationPolicy(metav1.DeletePropagationBackground)
	if err := env.Client.Delete(t.Context(), job, background); err != nil {
		t.Fatal(err)
	}

	failed := env.WaitForPhase(t, created.Name, v1alpha1.PhaseFailed)
	condition := meta.FindStatusCondition(failed.Status.Conditions, v1alpha1.ConditionFailed)
	if condition == nil || condition.Reason != v1alpha1.ReasonAgentJobMissing {
		t.Errorf("Failed condition = %+v, want reason %s", condition, v1alpha1.ReasonAgentJobMissing)
	}
}

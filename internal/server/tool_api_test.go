package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
	"github.com/a-hagi613/pagerless/internal/tools"
)

// toolRequest sends one tool API request with the agent's two headers.
func toolRequest(
	t *testing.T, method, url, token, uid string, body io.Reader,
) (int, []byte) {
	t.Helper()
	return toolRequestWithKey(t, method, url, token, uid, "test-key", body)
}

// toolRequestWithKey sends one tool API request under a chosen idempotency key.
func toolRequestWithKey(
	t *testing.T, method, url, token, uid, key string, body io.Reader,
) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if uid != "" {
		request.Header.Set(agentapi.PageUIDHeader, uid)
		request.Header.Set(agentapi.PageUIDHeaderCompat, uid)
	}
	if body != nil {
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload
}

// investigationURL is the manifest endpoint of a Page.
func investigationURL(env *testenv.Env, page string) string {
	return env.ReportURL + agentapi.InvestigationPath(page)
}

// toolURL is the tool endpoint of a Page.
func toolURL(env *testenv.Env, page, tool string) string {
	return env.ReportURL + agentapi.ToolPath(page, tool)
}

// TestTheManifestArrivesInOneAuthenticatedCall covers the one-call manifest and
// the two attribution checks.
func TestTheManifestArrivesInOneAuthenticatedCall(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	status, payload := toolRequest(t, http.MethodGet, investigationURL(env, created.Name),
		agent.Token, string(page.UID), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, payload)
	}
	var manifest tools.Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	if manifest.Page.UID != string(page.UID) || manifest.Page.Name != page.Name {
		t.Errorf("manifest Page = %+v, want the Page's identity", manifest.Page)
	}
	if len(manifest.Tools) != len(tools.Names()) {
		t.Errorf("manifest catalogue has %d tools, want the server catalogue", len(manifest.Tools))
	}
	for i, name := range tools.Names() {
		if manifest.Tools[i].Name != name {
			t.Errorf("manifest tool[%d] = %q, want %q", i, manifest.Tools[i].Name, name)
		}
	}
	if manifest.Limits.ToolCalls != agentapi.DefaultToolCallBudget ||
		manifest.Limits.ModelCalls != agentapi.DefaultModelCallBudget ||
		manifest.Limits.InputTokens != agentapi.DefaultInputTokenBudget ||
		manifest.Limits.Seconds != float64(agentapi.DefaultWallClockBudget) {
		t.Errorf("limits = %+v, want the four documented budgets", manifest.Limits)
	}
	if len(manifest.Scope.Namespaces) != 1 || manifest.Scope.Namespaces[0] != testenv.SandboxNamespace {
		t.Errorf("namespaces = %v, want the Target's namespace", manifest.Scope.Namespaces)
	}
	if string(manifest.Context) == "" || string(manifest.Context) == "null" {
		t.Errorf("context = %s, want a bundle document", manifest.Context)
	}

	other := env.CreatePage(t, testenv.NewPage("cart"))
	otherAgent := env.StartAgent(t, other.Name)
	t.Run("another Page's token", func(t *testing.T) {
		status, _ := toolRequest(t, http.MethodGet, investigationURL(env, created.Name),
			otherAgent.Token, string(page.UID), nil)
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, want a refusal for another Page's token", status)
		}
	})
	t.Run("a UID header that disagrees", func(t *testing.T) {
		status, _ := toolRequest(t, http.MethodGet, investigationURL(env, created.Name),
			agent.Token, string(other.UID), nil)
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a UID header that is not the Page's", status)
		}
	})
	t.Run("no UID header", func(t *testing.T) {
		status, _ := toolRequest(t, http.MethodGet, investigationURL(env, created.Name), agent.Token, "", nil)
		if status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 without the UID header", status)
		}
	})
}

// TestTheToolAPIRefusesAWriteToolAndRecordsTheAttempt covers the SuspiciousOutput
// event on the Page.
func TestTheToolAPIRefusesAWriteToolAndRecordsTheAttempt(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	status, payload := toolRequest(t, http.MethodPost, toolURL(env, created.Name, "k8s_delete_pod"),
		agent.Token, string(page.UID), strings.NewReader(`{"arguments":{"name":"checkout"}}`))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", status, payload)
	}
	var refusal tools.Refusal
	if err := json.Unmarshal(payload, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error.Code != tools.CodeUnknownTool {
		t.Errorf("code = %q, want %q", refusal.Error.Code, tools.CodeUnknownTool)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		latest := env.Get(t, created.Name)
		if len(latest.Status.SuspiciousOutputs) > 0 {
			if !strings.Contains(latest.Status.SuspiciousOutputs[0], "k8s_delete_pod") {
				t.Errorf("event = %q, want it to name the attempted tool", latest.Status.SuspiciousOutputs[0])
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the Page records no SuspiciousOutput for the write attempt")
}

// TestTheToolAPIRefusesTheSixteenthCall covers the server-side call budget and
// the call record it keeps.
func TestTheToolAPIRefusesTheSixteenthCall(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)
	body := `{"arguments":{"kind":"Pod"}}`
	for i := 1; i <= agentapi.DefaultToolCallBudget; i++ {
		status, payload := toolRequestWithKey(t, http.MethodPost, toolURL(env, created.Name, tools.ToolObjectList),
			agent.Token, string(page.UID), fmt.Sprintf("key-%d", i), strings.NewReader(body))
		if status != http.StatusOK {
			t.Fatalf("call %d: status = %d, body = %s", i, status, payload)
		}
	}
	status, payload := toolRequestWithKey(t, http.MethodPost, toolURL(env, created.Name, tools.ToolObjectList),
		agent.Token, string(page.UID), "key-over", strings.NewReader(body))
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s, want 429", status, payload)
	}
	var refusal tools.Refusal
	if err := json.Unmarshal(payload, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error.Code != tools.CodeBudgetExhausted {
		t.Errorf("code = %q, want %q", refusal.Error.Code, tools.CodeBudgetExhausted)
	}
	latest := env.Get(t, created.Name)
	if latest.Status.ToolCallCount < agentapi.DefaultToolCallBudget {
		t.Errorf("tool calls recorded = %d, want %d", latest.Status.ToolCallCount, agentapi.DefaultToolCallBudget)
	}
}

// TestTheToolAPIAnswersWithTypedJSON covers the {ok, data, meta} shape and the
// citation identifier a Finding uses.
func TestTheToolAPIAnswersWithTypedJSON(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	first := func(key string) map[string]any {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			toolURL(env, created.Name, tools.ToolObjectList), strings.NewReader(`{"arguments":{"kind":"Event"}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+agent.Token)
		request.Header.Set(agentapi.PageUIDHeader, string(page.UID))
		request.Header.Set("Idempotency-Key", key)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	firstBody := first("key-one")
	if firstBody["ok"] != true {
		t.Fatalf("body = %v, want ok true", firstBody)
	}
	if _, exists := firstBody["data"].(map[string]any); !exists {
		t.Errorf("data = %T, want typed JSON", firstBody["data"])
	}
	metadata, _ := firstBody["meta"].(map[string]any)
	if metadata["callId"] == "" || metadata["callId"] == nil {
		t.Errorf("meta = %v, want a callId a Finding can cite", firstBody["meta"])
	}
	secondBody := first("key-one")
	secondMeta, _ := secondBody["meta"].(map[string]any)
	if secondMeta["callId"] != metadata["callId"] {
		t.Errorf("a retry produced callId %v, want %v", secondMeta["callId"], metadata["callId"])
	}
}

// TestTheToolAPIRequiresAnIdempotencyKey covers the required header.
func TestTheToolAPIRequiresAnIdempotencyKey(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		toolURL(env, created.Name, tools.ToolObjectList), bytes.NewReader([]byte(`{"arguments":{"kind":"Pod"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+agent.Token)
	request.Header.Set(agentapi.PageUIDHeader, string(page.UID))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without an Idempotency-Key", response.StatusCode)
	}
}

// TestAV2ReportIsAcceptedOnlyWithARealCitation exercises the report path end to
// end: a call is made, its callId and a pointer into its data are cited, and the
// Page records the cited Finding. A fabricated citation is refused.
func TestAV2ReportIsAcceptedOnlyWithARealCitation(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	status, payload := toolRequestWithKey(t, http.MethodPost, toolURL(env, created.Name, tools.ToolObjectList),
		agent.Token, string(page.UID), "report-call", strings.NewReader(`{"arguments":{"kind":"Event"}}`))
	if status != http.StatusOK {
		t.Fatalf("tool call: status = %d, body = %s", status, payload)
	}
	var result struct {
		Meta tools.Meta `json:"meta"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}

	report := agentapi.Report{
		Page:      &agentapi.PageIdentity{Name: page.Name, UID: string(page.UID), Namespace: page.Namespace},
		Diagnosis: "The Target is crash-looping because its memory limit is too low.",
		Findings: []v1alpha1.Finding{{
			Kind: v1alpha1.FindingKindEvent, Source: "object_list Event",
			Summary: "the Event stream shows the BackOff", ToolCallID: result.Meta.CallID, Pointer: "/data/items",
		}},
		Proposal: agentapi.ReportProposal{
			Kind: agentapi.ProposalKindDiagnosisOnly, Reason: "operator-action-required",
			Recommendation: "Raise the memory limit.",
		},
		Usage:        &v1alpha1.Usage{InputTokens: 1200, OutputTokens: 200},
		Capabilities: &agentapi.Capabilities{UsageSource: "provider", StructuredOutput: map[string]string{"diagnose": "json_schema"}},
	}
	answer, err := agent.API.PostReport(t.Context(), report)
	if err != nil {
		t.Fatal(err)
	}
	if answer.StatusCode != http.StatusOK || answer.Report == nil || !answer.Report.Accepted {
		t.Fatalf("report answer = %+v, want it accepted", answer)
	}
	proposed := env.Get(t, created.Name)
	if proposed.Status.Phase != v1alpha1.PhaseProposed {
		t.Errorf("phase = %s, want Proposed", proposed.Status.Phase)
	}
	if len(proposed.Status.Findings) != 1 || proposed.Status.Findings[0].ToolCallID != result.Meta.CallID {
		t.Errorf("findings = %+v, want the cited Finding", proposed.Status.Findings)
	}
	if proposed.Status.StructuredOutput["diagnose"] != "json_schema" {
		t.Errorf("structured output = %v, want the recorded rung", proposed.Status.StructuredOutput)
	}

	fabricated := report
	fabricated.Findings = []v1alpha1.Finding{{
		Kind: v1alpha1.FindingKindEvent, Source: "invented", Summary: "invented",
		ToolCallID: "call-does-not-exist", Pointer: "/data/items",
	}}
	second := env.CreatePage(t, testenv.NewPage("cart"))
	secondAgent := env.StartAgent(t, second.Name)
	secondPage := env.Get(t, second.Name)
	fabricated.Page = &agentapi.PageIdentity{Name: secondPage.Name, UID: string(secondPage.UID), Namespace: secondPage.Namespace}
	refused, err := secondAgent.API.PostReport(t.Context(), fabricated)
	if err != nil {
		t.Fatal(err)
	}
	if refused.StatusCode != http.StatusUnprocessableEntity || refused.Report == nil {
		t.Fatalf("answer = %+v, want 422 for a fabricated citation", refused)
	}
	found := false
	for _, item := range refused.Report.Errors {
		if item.Field == "findings[0].toolCallId" {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %+v, want findings[0].toolCallId", refused.Report.Errors)
	}
	if got := env.Get(t, second.Name).Status.Phase; got != v1alpha1.PhaseInvestigating {
		t.Errorf("phase = %s after a refused report, want Investigating", got)
	}
}

// TestABudgetExhaustedAgentEndsThePageAsFailedKeepingFindings covers the server
// half of the budget contract: the agent exits 2, and the Page keeps the
// Findings it posted as steps.
func TestABudgetExhaustedAgentEndsThePageAsFailedKeepingFindings(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreatePage(t, testenv.NewPage(checkout))
	agent := env.StartAgent(t, created.Name)
	page := env.Get(t, created.Name)

	finding := v1alpha1.Finding{
		Kind: v1alpha1.FindingKindLog, Source: "pods_logs", Summary: "the container was OOMKilled",
	}
	if answer, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindFinding, Summary: "a Finding before the budget ran out", Finding: &finding,
		ToolCallID: "call-1",
	}); err != nil || answer.StatusCode != http.StatusAccepted {
		t.Fatalf("post the Finding step: %+v, %v", answer, err)
	}

	job := page.Status.AgentJobName
	pod := env.CreatePodFor(t, mustJob(t, env, job), server.DefaultAgentServiceAccount)
	var latest corev1.Pod
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(pod), &latest); err != nil {
		t.Fatal(err)
	}
	latest.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}},
	}}
	if err := env.Client.Status().Update(t.Context(), &latest); err != nil {
		t.Fatal(err)
	}
	env.EndJob(t, job, testenv.JobFailed)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		latestPage := env.Get(t, created.Name)
		if latestPage.Status.Phase == v1alpha1.PhaseFailed {
			if len(latestPage.Status.Findings) != 1 {
				t.Errorf("findings = %+v, want the Finding gathered before the budget ran out", latestPage.Status.Findings)
			}
			if condition := meta.FindStatusCondition(latestPage.Status.Conditions, v1alpha1.ConditionFailed); condition == nil ||
				condition.Reason != v1alpha1.ReasonBudgetExhausted {
				t.Errorf("Failed condition = %+v, want reason %s", condition, v1alpha1.ReasonBudgetExhausted)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("phase = %s, want Failed for an exhausted budget", env.Get(t, created.Name).Status.Phase)
}

// mustJob reads the Page's agent Job.
func mustJob(t *testing.T, env *testenv.Env, name string) *appsv1.Job {
	t.Helper()
	job, err := env.GetJob(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

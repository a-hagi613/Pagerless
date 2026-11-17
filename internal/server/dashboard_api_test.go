package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// The API's filter names and source values, so the tests spell each once.
const (
	workloadFilter     = workloadField
	triggerFilter      = "trigger"
	phaseFilter        = "phase"
	investigatingValue = "Investigating"
	historySource      = "history"
	liveSource         = "live"
	recordedFinding    = "recorded a log finding"
	metricsPanel       = "metrics"
	logsPanel          = "logs"
)

// dashboardGet performs a GET against the dashboard API and returns the response and its body.
func dashboardGet(t *testing.T, env *testenv.Env, target string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(env.DashboardURL + target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to GET %s: %v", target, err)
	}
	return response, body
}

// dashboardList reads the Triage list with the filters.
func dashboardList(t *testing.T, env *testenv.Env, filters url.Values) dashboardapi.ListResponse {
	t.Helper()
	target := "/api/triages"
	if encoded := filters.Encode(); encoded != "" {
		target += "?" + encoded
	}
	response, body := dashboardGet(t, env, target)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, response.StatusCode, body)
	}
	var list dashboardapi.ListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode the list: %v: %s", err, body)
	}
	return list
}

// dashboardDetail reads one Triage's record.
func dashboardDetail(t *testing.T, env *testenv.Env, name string) dashboardapi.DetailResponse {
	t.Helper()
	response, body := dashboardGet(t, env, "/api/triages/"+url.PathEscape(name))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the record of %s = %d: %s", name, response.StatusCode, body)
	}
	var record dashboardapi.DetailResponse
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatalf("decode the record: %v: %s", err, body)
	}
	return record
}

// names returns the names of the list rows, in order.
func names(list dashboardapi.ListResponse) []string {
	result := make([]string, 0, len(list.Triages))
	for _, triage := range list.Triages {
		result = append(result, triage.Name)
	}
	return result
}

// createInvestigating creates a Triage with the Trigger, waits for the server to start it, and returns it.
func createInvestigating(
	t *testing.T, env *testenv.Env, workload string, trigger v1alpha1.TriggerSource,
) *v1alpha1.Triage {
	t.Helper()
	candidate := testenv.NewTriage(workload)
	candidate.Spec.Trigger = v1alpha1.Trigger{Source: trigger}
	created := env.CreateTriage(t, candidate)
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
	return created
}

func TestDashboardListsTriagesMostRecentFirstWithFilters(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	first := createInvestigating(t, env, checkout, v1alpha1.TriggerSourceCLI)
	// A Workload has at most one Active Triage, so the first checkout Triage ends before the third starts. Its
	// Failed phase also keeps it in the list, which the history fallback below relies on.
	env.EndJob(t, env.Get(t, first.Name).Status.JobName, testenv.JobDeadlineExceeded)
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseFailed)
	env.Clock.Step(time.Minute)
	second := createInvestigating(t, env, "cart", v1alpha1.TriggerSourceAlertmanager)
	env.Clock.Step(time.Minute)
	third := createInvestigating(t, env, checkout, v1alpha1.TriggerSourceCLI)

	eventually(t, "the dashboard lists every Triage", func() bool {
		return len(dashboardList(t, env, nil).Triages) == 3
	})
	list := dashboardList(t, env, nil)
	if got, want := names(list), []string{third.Name, second.Name, first.Name}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("list order = %v, want the most recent Triage first %v", got, want)
	}
	for _, row := range list.Triages {
		if row.Source != liveSource || row.CreatedAt.IsZero() {
			t.Errorf("row %+v, want a live row with a creation time", row)
		}
	}

	for _, test := range []struct {
		name    string
		filters url.Values
		want    []string
	}{
		{name: "Workload", filters: url.Values{workloadFilter: {checkout}}, want: []string{third.Name, first.Name}},
		{name: "Trigger source", filters: url.Values{triggerFilter: {"Alertmanager"}}, want: []string{second.Name}},
		{name: "Phase", filters: url.Values{phaseFilter: {investigatingValue}}, want: []string{third.Name, second.Name}},
		{name: "a Workload and a Trigger", filters: url.Values{
			workloadFilter: {checkout}, triggerFilter: {"CLI"}, phaseFilter: {investigatingValue},
		}, want: []string{third.Name}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := names(dashboardList(t, env, test.filters))
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Errorf("filtered list = %v, want %v", got, test.want)
			}
		})
	}

	t.Run("the time range excludes an older Triage", func(t *testing.T) {
		future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		if got := names(dashboardList(t, env, url.Values{"since": {future}})); len(got) != 0 {
			t.Errorf("list since %s = %v, want no Triages", future, got)
		}
		if got := names(dashboardList(t, env, url.Values{"since": {"24h"}})); len(got) != 3 {
			t.Errorf("list since 24h = %v, want all three Triages", got)
		}
	})

	t.Run("a bad filter is refused", func(t *testing.T) {
		for _, target := range []string{
			"/api/triages?phase=Exploded",
			"/api/triages?trigger=Telepathy",
			"/api/triages?since=yesterday",
		} {
			response, body := dashboardGet(t, env, target)
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400: %s", target, response.StatusCode, body)
			}
		}
	})

	// Retention deletes the resource but not its history, so the row must remain with its Workload and Trigger.
	if err := env.Client.Delete(t.Context(), first); err != nil {
		t.Fatalf("delete Triage %s: %v", first.Name, err)
	}
	env.WaitForGone(t, first.Name)
	eventually(t, "the deleted Triage is listed from history", func() bool {
		for _, row := range dashboardList(t, env, nil).Triages {
			if row.Name == first.Name {
				return row.Source == historySource && row.Workload.Name == checkout &&
					row.Trigger.Source == v1alpha1.TriggerSourceCLI
			}
		}
		return false
	})
}

func TestDashboardDetailFallsBackToHistory(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := createInvestigating(t, env, checkout, v1alpha1.TriggerSourceAlertmanager)
	agent := env.StartAgent(t, created.Name)
	finding := logFinding
	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindFinding, Summary: recordedFinding, Finding: &finding,
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the finding: %+v, %v, want 202", result, err)
	}
	if result, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)

	if err := env.Client.Delete(t.Context(), created); err != nil {
		t.Fatalf("delete Triage: %v", err)
	}
	env.WaitForGone(t, created.Name)
	eventually(t, "the dashboard falls back to history", func() bool {
		return dashboardDetail(t, env, created.Name).Source == historySource
	})

	record := dashboardDetail(t, env, created.Name)
	if record.Source != historySource {
		t.Errorf("source = %q, want history", record.Source)
	}
	if record.Workload.Name != checkout || record.Trigger.Source != v1alpha1.TriggerSourceAlertmanager {
		t.Errorf("record = %+v, want the Workload and Trigger from history", record)
	}
	if record.Phase != v1alpha1.PhaseProposed {
		t.Errorf("phase = %s, want Proposed", record.Phase)
	}
	if record.Diagnosis == "" || record.Proposal == nil || record.Proposal.Action != v1alpha1.ActionNoAction {
		t.Errorf("record = %+v, want the Diagnosis and Proposal from history", record)
	}
	if len(record.Steps) != 1 || record.Steps[0].Summary != recordedFinding {
		t.Errorf("steps = %+v, want the step from history", record.Steps)
	}
	if len(record.Findings) != 1 || record.Findings[0] != logFinding {
		t.Errorf("findings = %+v, want the recorded Finding", record.Findings)
	}

	response, _ := dashboardGet(t, env, "/api/triages/absent")
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("GET an absent Triage = %d, want 404", response.StatusCode)
	}
}

func TestDashboardDetailShowsTheWholeTriageRecord(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	name := recovered(t, env)
	narrative := env.StartNarrative(t, name)
	written := "The checkout Deployment was failing on a low memory limit. The approved change raised it, and " +
		"Verification found the Workload recovered."
	if result, err := narrative.API.PostNarrative(t.Context(),
		agentapi.NarrativeReport{Narrative: written}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the Narrative: %+v, %v, want 202", result, err)
	}
	eventually(t, "the dashboard shows the Narrative", func() bool {
		return dashboardDetail(t, env, name).Narrative == written
	})

	requireTriageRecord(t, dashboardDetail(t, env, name), written)
}

// requireTriageRecord checks every part of a completed Triage's record.
func requireTriageRecord(t *testing.T, record dashboardapi.DetailResponse, narrative string) {
	t.Helper()
	requireTriageIdentity(t, record)
	requireProposal(t, record.Proposal)
	requireExecution(t, record)
	requireVerification(t, record.Verification)
	if record.Narrative != narrative {
		t.Errorf("narrative = %q, want %q", record.Narrative, narrative)
	}
	requireGrafana(t, record.Grafana)
}

// requireTriageIdentity checks the Workload, Trigger, Phase, Diagnosis and Finding of a completed record.
func requireTriageIdentity(t *testing.T, record dashboardapi.DetailResponse) {
	t.Helper()
	if record.Source != liveSource || record.Phase != v1alpha1.PhaseRecovered {
		t.Errorf("record = %s in %s, want the live Recovered Triage", record.Source, record.Phase)
	}
	if record.Workload.Name != checkout || record.Trigger.Source != v1alpha1.TriggerSourceCLI {
		t.Errorf("workload = %+v trigger = %+v, want the checkout Triage", record.Workload, record.Trigger)
	}
	if record.Diagnosis == "" {
		t.Error("the record has no Diagnosis")
	}
	if len(record.Findings) == 0 || record.Findings[0].Source != logFinding.Source {
		t.Errorf("findings = %+v, want the cited log Finding", record.Findings)
	}
}

// requireProposal checks the rendered change that the approver reviews.
func requireProposal(t *testing.T, proposal *v1alpha1.Proposal) {
	t.Helper()
	if proposal == nil || proposal.Action != v1alpha1.ActionPatchResources {
		t.Fatalf("proposal = %+v, want the PatchResources Proposal", proposal)
	}
	if proposal.RenderedChange == nil || proposal.RenderedChange.Path == "" ||
		!strings.Contains(proposal.RenderedChange.Patch, "memory") {
		t.Errorf("rendered change = %+v, want the memory patch", proposal.RenderedChange)
	}
	if proposal.Hash == "" || proposal.ExpiresAt.IsZero() {
		t.Errorf("proposal = %+v, want a hash and an expiry", proposal)
	}
}

// requireExecution checks the decision and the Executor's result.
func requireExecution(t *testing.T, record dashboardapi.DetailResponse) {
	t.Helper()
	execution := record.Execution
	if execution == nil || execution.PullRequestURL == "" || execution.MergeCommit == "" ||
		execution.ExecutedAt == nil {
		t.Errorf("execution = %+v, want the merged pull request", execution)
	}
	if record.Decision == nil || record.Decision.Outcome != v1alpha1.DecisionApproved {
		t.Errorf("decision = %+v, want the Approval", record.Decision)
	}
}

// requireVerification checks the Verification timeline and its four checks.
func requireVerification(t *testing.T, verification *dashboardapi.Verification) {
	t.Helper()
	if verification == nil || verification.StartedAt == nil {
		t.Fatalf("verification = %+v, want the timeline", verification)
	}
	if got := len(verification.Checks); got != 4 {
		t.Errorf("verification checks = %d, want 4", got)
	}
}

// requireGrafana checks the embedded panels and their deep links.
func requireGrafana(t *testing.T, grafana *dashboardapi.GrafanaLinks) {
	t.Helper()
	if grafana == nil || grafana.Metrics == nil || grafana.Logs == nil {
		t.Fatalf("grafana = %+v, want the metrics and logs panels", grafana)
	}
	if grafana.BaseURL != "http://grafana.monitoring.test" {
		t.Errorf("Grafana base URL = %q, want the harness's Grafana", grafana.BaseURL)
	}
	for name, link := range map[string]string{
		metricsPanel: grafana.Metrics.EmbedURL, logsPanel: grafana.Logs.EmbedURL,
	} {
		parsed, err := url.Parse(link)
		if err != nil {
			t.Fatalf("%s embed URL does not parse: %v", name, err)
		}
		if parsed.Query().Get("var-namespace") != sandboxNamespace ||
			parsed.Query().Get("var-workload") != checkout {
			t.Errorf("%s embed URL = %s, want the checkout Workload", name, link)
		}
	}
	for name, link := range map[string]string{
		metricsPanel: grafana.Metrics.DeepLink, logsPanel: grafana.Logs.DeepLink,
	} {
		if !strings.HasPrefix(link, "http://grafana.monitoring.test/d/") {
			t.Errorf("%s deep link = %q, want the Grafana dashboard", name, link)
		}
	}
}

// sseMessage is one parsed Server-Sent Event.
type sseMessage struct {
	kind string
	data []byte
}

// readSSE reads events until the kind arrives.
func readSSE(t *testing.T, reader *bufio.Reader, kind dashboardapi.EventKind) sseMessage {
	t.Helper()
	var (
		current string
		data    []byte
	)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the %s event: %v", kind, err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			current = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: ")...)
		case line == "":
			if current == string(kind) {
				return sseMessage{kind: current, data: data}
			}
			current, data = "", nil
		}
	}
}

func TestDashboardStreamsStepsAndPhaseChanges(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := createInvestigating(t, env, checkout, v1alpha1.TriggerSourceCLI)
	agent := env.StartAgent(t, created.Name)
	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindProgress, Summary: "gathered the Workload status",
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the first step: %+v, %v, want 202", result, err)
	}
	// The snapshot is read through the server's cache, so wait until the dashboard records the step before the
	// stream opens; afterwards the test asserts live delivery.
	eventually(t, "the dashboard records the first step", func() bool {
		return len(dashboardDetail(t, env, created.Name).Steps) == 1
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, env.DashboardURL+"/api/triages/"+created.Name+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("open the event stream: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("the event stream = %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	reader := bufio.NewReader(response.Body)

	snapshot := readSSE(t, reader, dashboardapi.EventSnapshot)
	var record dashboardapi.DetailResponse
	if err := json.Unmarshal(snapshot.data, &record); err != nil {
		t.Fatalf("decode the snapshot: %v", err)
	}
	if len(record.Steps) != 1 || record.Steps[0].Summary != "gathered the Workload status" {
		t.Errorf("snapshot steps = %+v, want the step recorded before the stream opened", record.Steps)
	}

	if result, err := agent.API.PostEvent(t.Context(), agentapi.StepEvent{
		Kind: v1alpha1.StepKindToolCall, Tool: podsLogTool, Summary: "read the previous logs",
	}); err != nil || result.StatusCode != http.StatusAccepted {
		t.Fatalf("post the second step: %+v, %v, want 202", result, err)
	}
	step := readSSE(t, reader, dashboardapi.EventStep)
	var stepEvent dashboardapi.StepEvent
	if err := json.Unmarshal(step.data, &stepEvent); err != nil {
		t.Fatalf("decode the step event: %v", err)
	}
	if stepEvent.Step.Kind != v1alpha1.StepKindToolCall || stepEvent.Step.Tool != podsLogTool ||
		stepEvent.StepCount != 2 {
		t.Errorf("step event = %+v, want the second step", stepEvent)
	}

	if result, err := agent.API.PostReport(t.Context(), noActionReport()); err != nil || result.StatusCode != http.StatusOK {
		t.Fatalf("report: %+v, %v, want 200", result, err)
	}
	phase := readSSE(t, reader, dashboardapi.EventPhase)
	var phaseEvent dashboardapi.PhaseEvent
	if err := json.Unmarshal(phase.data, &phaseEvent); err != nil {
		t.Fatalf("decode the phase event: %v", err)
	}
	if phaseEvent.Phase != v1alpha1.PhaseProposed || phaseEvent.Terminal {
		t.Errorf("phase event = %+v, want the Proposed Phase", phaseEvent)
	}
}

func TestDashboardRefusesAnUnknownTriageStream(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, env.DashboardURL+"/api/triages/absent/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("open an absent stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("an absent stream = %d, want 404", response.StatusCode)
	}
}

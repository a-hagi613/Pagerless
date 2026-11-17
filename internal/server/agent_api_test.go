package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// TestAgentAPIRejectsCallersThatAreNotTheTriagesJob covers the authentication
// boundary: each caller below holds something that looks like a credential,
// and none may post to the Triage.
func TestAgentAPIRejectsCallersThatAreNotTheTriagesJob(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	ctx := t.Context()

	target := env.CreateTriage(t, testenv.NewTriage(checkout))
	targetAgent := env.StartAgent(t, target.Name)
	other := env.CreateTriage(t, testenv.NewTriage("cart"))
	otherAgent := env.StartAgent(t, other.Name)

	intruder := &corev1.ServiceAccount{Name: "intruder", Namespace: testenv.Namespace}
	if err := env.Client.Create(ctx, intruder); err != nil {
		t.Fatal(err)
	}
	intruderPod := testenv.NewPod("intruder", "intruder")
	if err := env.Client.Create(ctx, intruderPod); err != nil {
		t.Fatal(err)
	}
	// A pod of the agent service account that no Job controls.
	orphanPod := testenv.NewPod("orphan", "pagerless-agent")
	if err := env.Client.Create(ctx, orphanPod); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"malformed token", "not-a-token", http.StatusUnauthorized},
		{"token with another audience", env.TokenFor(t, targetAgent.Pod, "pagerless-agent", "https://kubernetes.default.svc"), http.StatusUnauthorized},
		{"token of another service account", env.TokenFor(t, intruderPod, "intruder", agentapi.Audience), http.StatusForbidden},
		{"agent token not bound to a pod", env.UnboundTokenFor(t, testenv.Namespace, "pagerless-agent", agentapi.Audience), http.StatusForbidden},
		{"agent token bound to a pod no Job controls", env.TokenFor(t, orphanPod, "pagerless-agent", agentapi.Audience), http.StatusForbidden},
		{"token of another Triage's Job", otherAgent.Token, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := env.ClientWithToken(target.Name, tc.token)
			if tc.token == "" {
				client.Token = nil
			}
			for _, post := range []func() (agentapi.Result, error){
				func() (agentapi.Result, error) {
					return client.PostEvent(ctx, agentapi.StepEvent{Kind: v1alpha1.StepKindProgress, Summary: "x"})
				},
				func() (agentapi.Result, error) { return client.PostReport(ctx, noActionReport()) },
			} {
				result, err := post()
				if err != nil {
					t.Fatal(err)
				}
				if result.StatusCode != tc.want {
					t.Errorf("status = %d, want %d", result.StatusCode, tc.want)
				}
			}
			tr := env.Get(t, target.Name)
			if tr.Status.Phase != v1alpha1.PhaseInvestigating || tr.Status.StepCount != 0 || tr.Status.Diagnosis != "" {
				t.Errorf("a rejected caller changed the Triage: %+v", tr.Status)
			}
		})
	}

	t.Run("unknown Triage", func(t *testing.T) {
		result, err := env.ClientWithToken("no-such-triage", targetAgent.Token).PostReport(ctx, noActionReport())
		if err != nil {
			t.Fatal(err)
		}
		if result.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", result.StatusCode)
		}
	})
}

func TestAgentAPIRefusesMalformedAndOversizedBodies(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	agent := env.StartAgent(t, created.Name)
	path := agentapi.ReportPath(created.Name)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"not JSON", "diagnosis: cpu", http.StatusBadRequest},
		{"unknown field", `{"diagnosis":"d","findings":[],"proposal":{"action":"NoAction"},"approve":true}`, http.StatusBadRequest},
		{"trailing data", `{"diagnosis":"d","findings":[],"proposal":{"action":"NoAction"}} {}`, http.StatusBadRequest},
		{"oversized", `{"diagnosis":"` + strings.Repeat("x", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := agent.API.PostRaw(t.Context(), path, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", result.StatusCode, tc.want)
			}
		})
	}
	if got := env.Get(t, created.Name).Status.Phase; got != v1alpha1.PhaseInvestigating {
		t.Errorf("phase = %s, want Investigating", got)
	}
}

// TestReportsThatFailValidationAreAnsweredSynchronously covers the report
// contract's refusals. Only NoAction is accepted in this phase.
func TestReportsThatFailValidationAreAnsweredSynchronously(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	created := env.CreateTriage(t, testenv.NewTriage(checkout))
	agent := env.StartAgent(t, created.Name)

	valid := noActionReport()
	with := func(mutate func(*agentapi.Report)) agentapi.Report {
		r := valid
		r.Findings = append([]v1alpha1.Finding(nil), valid.Findings...)
		mutate(&r)
		return r
	}
	tests := []struct {
		name      string
		report    agentapi.Report
		wantField string
	}{
		{"no diagnosis", with(func(r *agentapi.Report) { r.Diagnosis = " " }), "diagnosis"},
		{"no findings", with(func(r *agentapi.Report) { r.Findings = nil }), "findings"},
		{"finding without a source", with(func(r *agentapi.Report) { r.Findings[0].Source = "" }), "findings[0].source"},
		{"finding of unknown kind", with(func(r *agentapi.Report) { r.Findings[0].Kind = "Rumour" }), "findings[0].kind"},
		{"unknown action", with(func(r *agentapi.Report) { r.Proposal.Action = "DeleteNamespace" }), "proposal.action"},
		{"action not yet accepted", with(func(r *agentapi.Report) {
			r.Proposal = v1alpha1.ProposedAction{
				Action:     v1alpha1.ActionScale,
				Parameters: &v1alpha1.ActionParameters{Scale: &v1alpha1.ScaleParameters{Replicas: 3}},
			}
		}), "proposal.action"},
		{"parameters for the wrong action", with(func(r *agentapi.Report) {
			r.Proposal.Parameters = &v1alpha1.ActionParameters{Rollback: &v1alpha1.RollbackParameters{Commit: "abcdef1"}}
		}), "proposal.parameters.rollback"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Every refusal counts toward the two that end a Triage, so each case has its own.
			own := env.CreateTriage(t, testenv.NewTriage(fmt.Sprintf("%s-%d", checkout, i)))
			ownAgent := env.StartAgent(t, own.Name)
			result, err := ownAgent.API.PostReport(t.Context(), tc.report)
			if err != nil {
				t.Fatal(err)
			}
			if result.StatusCode != http.StatusUnprocessableEntity || result.Report == nil || result.Report.Accepted {
				t.Fatalf("answer = %+v, want 422 with errors", result)
			}
			found := false
			for _, e := range result.Report.Errors {
				found = found || e.Field == tc.wantField
			}
			if !found {
				t.Errorf("errors = %+v, want one for field %s", result.Report.Errors, tc.wantField)
			}
			tr := env.Get(t, own.Name)
			if tr.Status.Phase != v1alpha1.PhaseInvestigating || tr.Status.Proposal != nil {
				t.Errorf("a refused report changed the Triage: phase %s, proposal %+v", tr.Status.Phase, tr.Status.Proposal)
			}
		})
	}

	t.Run("a later valid report is accepted", func(t *testing.T) {
		result, err := agent.API.PostReport(t.Context(), valid)
		if err != nil || result.StatusCode != http.StatusOK {
			t.Fatalf("answer = %+v, error %v, want 200", result, err)
		}
		env.WaitForPhase(t, created.Name, v1alpha1.PhaseProposed)
	})
	t.Run("a second report is refused", func(t *testing.T) {
		result, err := agent.API.PostReport(t.Context(), valid)
		if err != nil {
			t.Fatal(err)
		}
		if result.StatusCode != http.StatusConflict {
			t.Errorf("status = %d, want 409", result.StatusCode)
		}
	})
}

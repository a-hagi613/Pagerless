package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// dashboardRequest performs an authenticated dashboard request. A non-empty
// body is sent as JSON, and the CSRF token is repeated in the header as the
// browser does.
func dashboardRequest(
	t *testing.T, env *testenv.Env, session testenv.DashboardSession, method, target string, body any,
) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the %s body: %v", target, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, env.DashboardURL+target, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	request.AddCookie(session.Cookie)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		request.Header.Set("X-CSRF-Token", session.CSRF)
	}
	response, err := (&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, target, err)
	}
	return response, answer
}

// requireStatus checks a response status and includes the body in the failure.
func requireStatus(t *testing.T, response *http.Response, body []byte, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("%s = %d, want %d: %s", response.Request.URL.Path, response.StatusCode, want, body)
	}
}

func TestDashboardReportsTheSignedInUser(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	response, body := dashboardRequest(t, env, env.Dashboard, http.MethodGet, "/api/session", nil)
	requireStatus(t, response, body, http.StatusOK)
	var session dashboardapi.SessionResponse
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode the session: %v: %s", err, body)
	}
	if !session.Authenticated || session.Username != testenv.DashboardUser {
		t.Errorf("session = %+v, want the signed-in %s", session, testenv.DashboardUser)
	}
	if !session.CanApprove || session.CSRF == "" {
		t.Errorf("session = %+v, want approver status and a CSRF token", session)
	}
}

func TestDashboardRefusesUnauthenticatedAndUnprotectedRequests(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)

	request, err := http.NewRequest(http.MethodGet, env.DashboardURL+"/api/triages", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated API request = %d, want 401", response.StatusCode)
	}

	// The page redirects a browser to sign-in instead of answering 401.
	page, err := (&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Get(env.DashboardURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = page.Body.Close() }()
	if page.StatusCode != http.StatusSeeOther || page.Header.Get("Location") != "/login" {
		t.Errorf("an unauthenticated page request = %d %q, want 303 /login",
			page.StatusCode, page.Header.Get("Location"))
	}
}

func TestDashboardDecisionCreatesAnApprovalWithTheAssertedApprover(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(testenv.DashboardUser))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost,
		"/api/triages/"+proposed.Name+"/decision",
		dashboardapiDecision{Decision: string(v1alpha1.DecisionApproved), Reason: "raise the limit"})
	requireStatus(t, response, body, http.StatusCreated)
	var recorded map[string]string
	if err := json.Unmarshal(body, &recorded); err != nil {
		t.Fatalf("decode the decision answer: %v: %s", err, body)
	}
	if recorded["approver"] != testenv.DashboardUser {
		t.Errorf("answer = %v, want the asserted approver %s", recorded, testenv.DashboardUser)
	}

	tr := env.WaitForApproved(t, proposed.Name)
	got := tr.Status.Decision
	if got == nil || got.Outcome != v1alpha1.DecisionApproved || got.Approver != testenv.DashboardUser ||
		got.Reason != "raise the limit" {
		t.Errorf("decision = %+v, want an Approval by %s with its reason", got, testenv.DashboardUser)
	}
	var stored v1alpha1.TriageApprovalList
	if err := env.Client.List(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 1 {
		t.Fatalf("stored %d approvals, want one", len(stored.Items))
	}
	approval := stored.Items[0]
	if approval.Spec.Approver != testenv.DashboardUser {
		t.Errorf("approval approver = %q, want %q", approval.Spec.Approver, testenv.DashboardUser)
	}
	if approval.Annotations[admissionDecisionSource] != "dashboard" ||
		approval.Annotations[admissionApproverSubject] == "" {
		t.Errorf("annotations = %v, want the dashboard source and the Keycloak subject", approval.Annotations)
	}
}

func TestDashboardRejectionIsRecorded(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(testenv.DashboardUser))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost,
		"/api/triages/"+proposed.Name+"/decision",
		dashboardapiDecision{Decision: string(v1alpha1.DecisionRejected), Reason: "use 1Gi instead"})
	requireStatus(t, response, body, http.StatusCreated)
	tr := env.WaitForPhase(t, proposed.Name, v1alpha1.PhaseRejected)
	if got := tr.Status.Decision; got == nil || got.Approver != testenv.DashboardUser || got.Reason != "use 1Gi instead" {
		t.Errorf("decision = %+v, want the dashboard user's Rejection", got)
	}
}

func TestDashboardRefusesADecisionWithoutCSRFOrFromANonApprover(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources(), testenv.WithApprovers(testenv.DashboardUser))
	deploy(t, env, workload{name: checkout})
	proposed := proposedTriage(t, env, checkout)

	withoutToken := env.Dashboard
	withoutToken.CSRF = "wrong"
	response, body := dashboardRequest(t, env, withoutToken, http.MethodPost,
		"/api/triages/"+proposed.Name+"/decision", dashboardapiDecision{Decision: string(v1alpha1.DecisionApproved)})
	requireStatus(t, response, body, http.StatusForbidden)

	stranger := env.SignInDashboard(t, "mallory@example.com", false)
	response, body = dashboardRequest(t, env, stranger, http.MethodPost,
		"/api/triages/"+proposed.Name+"/decision", dashboardapiDecision{Decision: string(v1alpha1.DecisionApproved)})
	requireStatus(t, response, body, http.StatusForbidden)
	requireUndecided(t, env, proposed.Name, v1alpha1.PhaseProposed)
}

func TestDashboardStartsATriageForAChosenWorkloadWithAHint(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	deploy(t, env, workload{name: checkout})

	response, body := dashboardRequest(t, env, env.Dashboard, http.MethodGet, "/api/workloads", nil)
	requireStatus(t, response, body, http.StatusOK)
	var list dashboardapiWorkloads
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode the Workloads: %v: %s", err, body)
	}
	found := false
	for _, option := range list.Workloads {
		if option.Name == checkout && option.Kind == v1alpha1.WorkloadKindDeployment {
			found = true
		}
	}
	if !found {
		t.Fatalf("workloads = %+v, want the checkout Deployment", list.Workloads)
	}

	response, body = dashboardRequest(t, env, env.Dashboard, http.MethodPost, "/api/triages",
		dashboardapiStart{
			Workload: v1alpha1.WorkloadReference{
				Kind: v1alpha1.WorkloadKindDeployment, Namespace: sandboxNamespace, Name: checkout,
			},
			Hint: "check the database connection pool",
		})
	requireStatus(t, response, body, http.StatusCreated)
	var started map[string]any
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode the start answer: %v: %s", err, body)
	}
	name, _ := started["name"].(string)
	if name == "" {
		t.Fatalf("answer = %s, want a Triage name", body)
	}
	tr := env.WaitForPhase(t, name, v1alpha1.PhaseInvestigating)
	if tr.Spec.Trigger.Source != v1alpha1.TriggerSourceDashboard {
		t.Errorf("trigger = %+v, want the Dashboard source", tr.Spec.Trigger)
	}
	if tr.Spec.Hint != "check the database connection pool" {
		t.Errorf("hint = %q, want the submitted hint", tr.Spec.Hint)
	}
	if tr.Spec.Workload.Namespace != sandboxNamespace || tr.Spec.Workload.Name != checkout {
		t.Errorf("workload = %+v, want sandbox/%s", tr.Spec.Workload, checkout)
	}

	t.Run("a second Start Triage reports the Active Triage", func(t *testing.T) {
		response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost, "/api/triages",
			dashboardapiStart{
				Workload: v1alpha1.WorkloadReference{
					Kind: v1alpha1.WorkloadKindDeployment, Namespace: sandboxNamespace, Name: checkout,
				},
			})
		requireStatus(t, response, body, http.StatusOK)
		var answer struct {
			Name          string `json:"name"`
			AlreadyActive bool   `json:"alreadyActive"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode the answer: %v: %s", err, body)
		}
		if !answer.AlreadyActive || answer.Name != name {
			t.Errorf("answer = %+v, want the existing Triage %s", answer, name)
		}
	})

	t.Run("an unknown Workload is refused", func(t *testing.T) {
		response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost, "/api/triages",
			dashboardapiStart{
				Workload: v1alpha1.WorkloadReference{
					Kind: v1alpha1.WorkloadKindDeployment, Namespace: sandboxNamespace, Name: "absent",
				},
			})
		requireStatus(t, response, body, http.StatusNotFound)
	})

	t.Run("a namespace the dashboard does not offer is refused", func(t *testing.T) {
		response, body := dashboardRequest(t, env, env.Dashboard, http.MethodPost, "/api/triages",
			dashboardapiStart{
				Workload: v1alpha1.WorkloadReference{
					Kind: v1alpha1.WorkloadKindDeployment, Namespace: "kube-system", Name: checkout,
				},
			})
		requireStatus(t, response, body, http.StatusBadRequest)
	})
}

// The request and response shapes that the dashboard's TypeScript sends.
type (
	dashboardapiDecision = struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason,omitempty"`
	}
	dashboardapiStart = struct {
		Workload v1alpha1.WorkloadReference `json:"workload"`
		Hint     string                     `json:"hint,omitempty"`
	}
	dashboardapiWorkloads = struct {
		Workloads []v1alpha1.WorkloadReference `json:"workloads"`
	}
)

package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/alerts"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	sandboxNamespace = "sandbox"
	optInLabel       = "pagerless_page"
)

// webhookBody is the Alertmanager webhook payload, version 4, that the receiver accepts.
type webhookBody struct {
	Version  string         `json:"version"`
	GroupKey string         `json:"groupKey"`
	Status   string         `json:"status"`
	Receiver string         `json:"receiver"`
	Alerts   []webhookAlert `json:"alerts"`
}

// webhookAlert is one alert in an Alertmanager webhook payload.
type webhookAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// optedInAlert returns a firing alert that carries the opt-in label and the
// given labels, as a rule for a Target's OOMKilled containers would.
func optedInAlert(fingerprint string, labels map[string]string) webhookAlert {
	all := map[string]string{
		"alertname": "OOMKilled",
		"namespace": sandboxNamespace,
		optInLabel:  "true",
	}
	maps.Copy(all, labels)
	return webhookAlert{
		Status:      "firing",
		Labels:      all,
		Annotations: map[string]string{"summary": "container OOMKilled"},
		StartsAt:    testenv.StartTime,
		Fingerprint: fingerprint,
	}
}

// checkoutAlert returns an opted-in alert for the checkout Deployment.
func checkoutAlert(fingerprint, pod string) webhookAlert {
	return optedInAlert(fingerprint, map[string]string{"deployment": checkout, "pod": pod})
}

// postAlerts posts one firing Alertmanager payload to the receiver and returns the status code.
func postAlerts(t *testing.T, env *testenv.Env, items ...webhookAlert) int {
	t.Helper()
	return postAlertsWithToken(t, env, testenv.AlertmanagerToken, items...)
}

func postAlertsWithToken(t *testing.T, env *testenv.Env, token string, items ...webhookAlert) int {
	t.Helper()
	body, err := json.Marshal(webhookBody{
		Version:  "4",
		GroupKey: `{}:{alertname="OOMKilled"}`,
		Status:   "firing",
		Receiver: "pagerless",
		Alerts:   items,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.ReportURL+alerts.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post alerts: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// pagesInNamespace lists the Pages of the harness namespace.
func pagesInNamespace(t *testing.T, env *testenv.Env) []v1alpha1.Page {
	t.Helper()
	var list v1alpha1.PageList
	if err := env.Client.List(t.Context(), &list, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatalf("list Pages: %v", err)
	}
	return list.Items
}

func TestAlertReceiverRefusesUnauthenticatedTriggers(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))
	for _, authorization := range []string{"", "Bearer wrong-token", "Basic wrong-token"} {
		body, err := json.Marshal(webhookBody{Alerts: []webhookAlert{checkoutAlert("fp-unauthorized", "checkout-1")}})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			env.ReportURL+alerts.Path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", authorization)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("authorization %q: status = %d, want 401", authorization, resp.StatusCode)
		}
	}
	if count := len(pagesInNamespace(t, env)); count != 0 {
		t.Errorf("unauthenticated requests created %d Pages", count)
	}
}

func TestAlertReceiverReadsTheRotatedTokenForEveryDelivery(t *testing.T) {
	t.Parallel()
	var expected atomic.Value
	expected.Store(testenv.AlertmanagerToken)
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace),
		testenv.WithAlertmanagerToken(func() (string, error) { return expected.Load().(string), nil }))
	if code := postAlerts(t, env, checkoutAlert("fp-before", "checkout-1")); code != http.StatusOK {
		t.Fatalf("before rotation: status = %d, want 200", code)
	}
	expected.Store("rotated-alertmanager-token")
	if code := postAlerts(t, env, checkoutAlert("fp-after", "checkout-1")); code != http.StatusUnauthorized {
		t.Fatalf("old token after rotation: status = %d, want 401", code)
	}
	items := pagesInNamespace(t, env)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want only the authenticated delivery", len(items))
	}
	tr := env.WaitForPhase(t, items[0].Name, v1alpha1.PhaseInvestigating)
	if tr.Status.TriggerCount != 1 {
		t.Errorf("unauthorized delivery changed trigger count to %d", tr.Status.TriggerCount)
	}
	code := postAlertsWithToken(t, env, "rotated-alertmanager-token", checkoutAlert("fp-new", "checkout-1"))
	if code != http.StatusOK {
		t.Errorf("new token after rotation: status = %d, want 200", code)
	}
	env.WaitFor(t, tr.Name, "the rotated token's delivery is recorded", func(latest *v1alpha1.Page) bool {
		return latest.Status.TriggerCount == 2
	})
	// A Secret that is momentarily empty must fail closed rather than accepting an empty bearer token.
	expected.Store("")
	if code := postAlerts(t, env, checkoutAlert("fp-empty", "checkout-1")); code != http.StatusServiceUnavailable {
		t.Errorf("empty expected token: status = %d, want 503", code)
	}
}

func TestAlertReceiverFailsClosedWhenTheSecretCannotBeRead(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace),
		testenv.WithAlertmanagerToken(func() (string, error) { return "", errors.New("unavailable") }))
	if code := postAlerts(t, env, checkoutAlert("fp-unavailable", "checkout-1")); code != http.StatusServiceUnavailable {
		t.Errorf("unreadable token: status = %d, want 503", code)
	}
	if items := pagesInNamespace(t, env); len(items) != 0 {
		t.Errorf("a request without a readable expected token created %d Pages", len(items))
	}
}

func TestOptedInAlertStartsAPageForItsTarget(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	if code := postAlerts(t, env, checkoutAlert("fp-1", "checkout-7d9f8-x2k4q")); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	items := pagesInNamespace(t, env)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want 1", len(items))
	}
	tr := env.WaitForPhase(t, items[0].Name, v1alpha1.PhaseInvestigating)
	want := v1alpha1.TargetReference{Kind: v1alpha1.TargetKindDeployment, Namespace: sandboxNamespace, Name: checkout}
	if tr.Spec.Target != want {
		t.Errorf("target = %+v, want %+v", tr.Spec.Target, want)
	}
	if tr.Spec.Trigger.Source != v1alpha1.TriggerSourceAlertmanager {
		t.Errorf("trigger source = %q, want Alertmanager", tr.Spec.Trigger.Source)
	}
	if tr.Spec.Trigger.Pod != "checkout-7d9f8-x2k4q" || tr.Spec.Trigger.AlertFingerprint != "fp-1" {
		t.Errorf("trigger = %+v, want the pod and fingerprint of the alert", tr.Spec.Trigger)
	}
	if tr.Status.TriggerCount != 1 {
		t.Errorf("triggerCount = %d, want 1", tr.Status.TriggerCount)
	}
}

func TestStatefulSetAlertTargetsAStatefulSet(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	ledger := optedInAlert("fp-db", map[string]string{"statefulset": "ledger", "pod": "ledger-0"})
	if code := postAlerts(t, env, ledger); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	items := pagesInNamespace(t, env)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want 1", len(items))
	}
	tr := env.WaitForPhase(t, items[0].Name, v1alpha1.PhaseInvestigating)
	want := v1alpha1.TargetReference{Kind: v1alpha1.TargetKindStatefulSet, Namespace: sandboxNamespace, Name: "ledger"}
	if tr.Spec.Target != want {
		t.Errorf("target = %+v, want %+v", tr.Spec.Target, want)
	}
}

func TestAlertsThatAreNotOptedInOrOutsideTheAllowlistAreIgnored(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	withoutLabel := checkoutAlert("fp-a", "checkout-0")
	delete(withoutLabel.Labels, optInLabel)
	notTrue := checkoutAlert("fp-b", "checkout-0")
	notTrue.Labels[optInLabel] = "false"
	outside := checkoutAlert("fp-c", "checkout-0")
	outside.Labels["namespace"] = "payments"
	resolved := checkoutAlert("fp-d", "checkout-0")
	resolved.Status = "resolved"
	resolved.EndsAt = testenv.StartTime.Add(time.Minute)

	for name, item := range map[string]webhookAlert{
		"without the opt-in label":        withoutLabel,
		"with a label other than true":    notTrue,
		"outside the namespace allowlist": outside,
		"already resolved":                resolved,
	} {
		if code := postAlerts(t, env, item); code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, code)
		}
		if n := len(pagesInNamespace(t, env)); n != 0 {
			t.Errorf("%s: %d Pages were created, want none", name, n)
		}
	}
}

func TestRepeatAlertJoinsTheActivePage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	// The two posts are not separated by a wait, so the second may arrive before
	// the reconciler has initialized the Page. Its count must still reach 2.
	if code := postAlerts(t, env, checkoutAlert("fp-1", "checkout-a")); code != http.StatusOK {
		t.Fatalf("first alert: status = %d, want 200", code)
	}
	if code := postAlerts(t, env, checkoutAlert("fp-2", "checkout-b")); code != http.StatusOK {
		t.Fatalf("repeat alert: status = %d, want 200", code)
	}

	items := pagesInNamespace(t, env)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want 1: a repeat alert must not start another Page", len(items))
	}
	tr := env.WaitFor(t, items[0].Name, "trigger count is 2", func(tr *v1alpha1.Page) bool {
		return tr.Status.TriggerCount == 2
	})
	if tr.Spec.Trigger.AlertFingerprint != "fp-1" {
		t.Errorf("fingerprint = %q, want the fingerprint of the alert that started the Page",
			tr.Spec.Trigger.AlertFingerprint)
	}
}

func TestRepeatAlertAfterTheActivePageEndedStartsANewPage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	if code := postAlerts(t, env, checkoutAlert("fp-1", "checkout-a")); code != http.StatusOK {
		t.Fatalf("first alert: status = %d, want 200", code)
	}
	first := env.WaitForPhase(t, pagesInNamespace(t, env)[0].Name, v1alpha1.PhaseInvestigating)
	env.EndJob(t, first.Status.AgentJobName, testenv.JobFailed)
	env.WaitForPhase(t, first.Name, v1alpha1.PhaseFailed)

	if code := postAlerts(t, env, checkoutAlert("fp-2", "checkout-b")); code != http.StatusOK {
		t.Fatalf("repeat alert: status = %d, want 200", code)
	}
	items := pagesInNamespace(t, env)
	if len(items) != 2 {
		t.Fatalf("Pages = %d, want 2: a new alert after the Active Page ended starts a Page", len(items))
	}
	for i := range items {
		if items[i].Name != first.Name {
			env.WaitForPhase(t, items[i].Name, v1alpha1.PhaseInvestigating)
		}
	}
}

func TestAlertReceiverRefusesMalformedBodiesAndOtherMethods(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))

	url := env.ReportURL + alerts.Path
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader([]byte("{")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testenv.AlertmanagerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", resp.StatusCode)
	}

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, env.ReportURL+alerts.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", resp.StatusCode)
	}
	if n := len(pagesInNamespace(t, env)); n != 0 {
		t.Errorf("%d Pages were created by refused requests, want none", n)
	}
}

package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/testenv"
	"github.com/a-hagi613/pagerless/internal/webhooks"
)

// alertPayload is one v2 webhook delivery.
type alertPayload struct {
	Status string              `json:"status"`
	Alerts []alertPayloadAlert `json:"alerts"`
}

// alertPayloadAlert is one alert of a delivery.
type alertPayloadAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// postAlert posts one Alertmanager delivery to the v2 route.
func postAlert(t *testing.T, env *testenv.Env, status, fingerprint string) int {
	t.Helper()
	body, err := json.Marshal(alertPayload{
		Status: status,
		Alerts: []alertPayloadAlert{{
			Status: status,
			Labels: map[string]string{
				"alertname": "OOMKilled", labelNamespace: sandboxNamespace, labelDeployment: checkout,
				labelPod: checkoutPod, "severity": "critical",
				"pagerless.dev/watch": labelTrue,
			},
			Annotations: map[string]string{"summary": "checkout is OOMKilling"},
			StartsAt:    testenv.StartTime.Format(time.RFC3339),
			Fingerprint: fingerprint,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		env.ReportURL+webhooks.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testenv.WebhookToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post alert: %v", err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestAnAlertmanagerPayloadJoinsTheWatchersPage(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))
	deployment := &appsv1.Deployment{
		Name: checkout, Namespace: sandboxNamespace,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: checkout}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelApp: checkout}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: checkout, Image: checkoutImage,
				}}},
			},
		},
	}
	if err := env.Client.Create(t.Context(), deployment); err != nil {
		t.Fatalf("create the Target: %v", err)
	}
	target := v1alpha1.TargetReference{
		APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: sandboxNamespace,
		Name: checkout, UID: string(deployment.UID),
	}
	watcherPage := env.CreatePage(t, &v1alpha1.Page{
		GenerateName: "page-", Namespace: testenv.Namespace,
		Spec: v1alpha1.PageSpec{
			Target: target, Fingerprint: v1alpha1.Fingerprint(target, "CrashLoopBackOff"),
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent, Reason: "CrashLoopBackOff"},
		},
	})
	env.WaitFor(t, watcherPage.Name, "the watcher's Page to become active", func(p *v1alpha1.Page) bool {
		return p.Status.Phase != "" && p.Status.Phase.IsActive()
	})

	if code := postAlert(t, env, alertFiring, "alertmanager-fingerprint-1"); code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200", code)
	}
	// A sender retry must not count a second Trigger.
	if code := postAlert(t, env, alertFiring, "alertmanager-fingerprint-1"); code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", code)
	}
	if items := pagesInNamespace(t, env); len(items) != 1 {
		t.Fatalf("Pages = %d, want the watcher's Page only", len(items))
	}
	joined := env.Get(t, watcherPage.Name)
	if joined.Status.TriggerCount != 2 {
		t.Errorf("trigger count = %d, want 2 (the watcher's Trigger and one alert)", joined.Status.TriggerCount)
	}
	if len(joined.Status.Alerts) != 1 {
		t.Fatalf("alerts = %d, want the accepted alert", len(joined.Status.Alerts))
	}
	alert := joined.Status.Alerts[0]
	if alert.Source != v1alpha1.TriggerSourceAlertmanager || alert.State != v1alpha1.AlertStateFiring {
		t.Errorf("alert = %+v, want a firing Alertmanager alert", alert)
	}
	if alert.Fingerprint != "alertmanager-fingerprint-1" || alert.Sender != "alertmanager" {
		t.Errorf("alert identity = %q/%q, want the sender and fingerprint", alert.Sender, alert.Fingerprint)
	}
	if joined.Status.Subject == nil || joined.Status.Subject.Resolution != "Explicit" &&
		joined.Status.Subject.Resolution != "WorkloadLabel" {
		t.Errorf("subject = %+v, want a resolved Target", joined.Status.Subject)
	}

	if code := postAlert(t, env, "resolved", "alertmanager-fingerprint-1"); code != http.StatusOK {
		t.Fatalf("resolved delivery status = %d, want 200", code)
	}
	closed := env.Get(t, watcherPage.Name)
	if len(closed.Status.Alerts) != 1 || closed.Status.Alerts[0].State != v1alpha1.AlertStateResolved {
		t.Errorf("alerts = %+v, want the one alert resolved", closed.Status.Alerts)
	}
	if closed.Status.Phase.IsTerminal() {
		t.Errorf("phase = %q, want the Page open while Kubernetes observations remain", closed.Status.Phase)
	}
}

func TestTheWebhookRefusesAnUnauthenticatedDelivery(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, testenv.WithAlertNamespaces(sandboxNamespace))
	body, err := json.Marshal(alertPayload{Status: alertFiring, Alerts: []alertPayloadAlert{{
		Status: alertFiring, Labels: map[string]string{labelNamespace: sandboxNamespace, labelDeployment: checkout,
			"pagerless.dev/watch": labelTrue}, Fingerprint: "unauthenticated",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		env.ReportURL+webhooks.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer wrong")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", response.StatusCode)
	}
	if items := pagesInNamespace(t, env); len(items) != 0 {
		t.Errorf("an unauthenticated delivery created %d Pages", len(items))
	}
}

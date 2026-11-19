package webhooks

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/subject"
)

const (
	testNamespace       = "pagerless"
	testShop            = "shop"
	testToken           = "webhook-token"
	testGrafana         = "grafana-secret"
	testCheckout        = "checkout"
	testTopicARN        = "arn:aws:sns:us-east-1:123456789012:pagerless-alarms"
	testAuthorization   = "Authorization"
	testSignatureHeader = "X-Pagerless-Signature"
	alertmanagerFixture = "alertmanager.json"
)

// memoryDeliveries is an in-memory DeliveryStore.
type memoryDeliveries struct {
	mutex   sync.Mutex
	records map[string]Delivery
}

// newMemoryDeliveries returns an empty store.
func newMemoryDeliveries() *memoryDeliveries {
	return &memoryDeliveries{records: make(map[string]Delivery)}
}

// HasDelivery reports whether the key is recorded.
func (m *memoryDeliveries) HasDelivery(_ context.Context, key string) (bool, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	_, ok := m.records[key]
	return ok, nil
}

// RecordDelivery records the key.
func (m *memoryDeliveries) RecordDelivery(_ context.Context, delivery Delivery) (bool, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if _, ok := m.records[delivery.Key]; ok {
		return false, nil
	}
	m.records[delivery.Key] = delivery
	return true, nil
}

// PruneDeliveries counts the rows a prune would remove.
func (m *memoryDeliveries) PruneDeliveries(_ context.Context, before time.Time) (int64, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	var removed int64
	for key, delivery := range m.records {
		if delivery.ReceivedAt.Before(before) {
			delete(m.records, key)
			removed++
		}
	}
	return removed, nil
}

// testReceiver builds a receiver over a fake client seeded with one live
// Deployment.
func testReceiver(t *testing.T, options ...func(*Receiver)) (*Receiver, client.Client, *memoryDeliveries) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{
		Name: testCheckout, Namespace: testShop, UID: "11111111-1111-1111-1111-111111111111",
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).
		WithStatusSubresource(&v1alpha1.Page{}).Build()
	deliveries := newMemoryDeliveries()
	clock := clocktesting.NewFakeClock(time.Date(2026, time.November, 16, 12, 5, 0, 0, time.UTC))
	receiver := &Receiver{
		Client:    fakeClient,
		Status:    &page.StatusStore{Client: fakeClient, Reader: fakeClient, Clock: clock},
		Namespace: testNamespace, Namespaces: []string{testShop},
		Resolver:          &subject.Resolver{Reader: fakeClient},
		Checker:           &subject.Checker{Reader: fakeClient},
		Deliveries:        deliveries,
		Clock:             clock,
		AlertmanagerToken: func() (string, error) { return testToken, nil },
		GrafanaSecret:     func() (string, error) { return testGrafana, nil },
		DatadogToken:      func() (string, error) { return testToken, nil },
		GenericToken:      func() (string, error) { return testToken, nil },
		SNSTopics:         []string{testTopicARN},
	}
	for _, option := range options {
		option(receiver)
	}
	return receiver, fakeClient, deliveries
}

// deliver posts one payload and returns the response.
func deliver(
	t *testing.T, receiver *Receiver, source Source, body []byte, headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, Path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SourceHeader, string(source))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	receiver.ServeHTTP(recorder, request)
	return recorder
}

// pages lists the Pages of the test namespace.
func pages(t *testing.T, fakeClient client.Client) []v1alpha1.Page {
	t.Helper()
	var list v1alpha1.PageList
	if err := fakeClient.List(t.Context(), &list, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// decodeResponse reads the bounded answer.
func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) response {
	t.Helper()
	var answer response
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return answer
}

func TestEveryDialectOpensAPageForItsSubject(t *testing.T) {
	t.Parallel()
	grafana := fixture(t, "grafana.json")
	mac := hmacSHA256([]byte(testGrafana), grafana)
	tests := []struct {
		name    string
		source  Source
		fixture string
		headers map[string]string
		want    v1alpha1.TriggerSource
		target  string
	}{
		{name: "alertmanager", source: SourceAlertmanager, fixture: alertmanagerFixture,
			headers: map[string]string{testAuthorization: "Bearer " + testToken},
			want:    v1alpha1.TriggerSourceAlertmanager, target: testCheckout},
		{name: "grafana", source: SourceGrafana, fixture: "grafana.json",
			headers: map[string]string{testSignatureHeader: "sha256=" + hex.EncodeToString(mac)},
			want:    v1alpha1.TriggerSourceGrafana, target: "orders"},
		{name: "datadog", source: SourceDatadog, fixture: "datadog.json",
			headers: map[string]string{testAuthorization: "Bearer " + testToken},
			want:    v1alpha1.TriggerSourceDatadog, target: testCheckout},
		{name: "sns", source: SourceSNS, fixture: "sns-notification.json",
			want: v1alpha1.TriggerSourceSNS, target: testCheckout},
		{name: "generic", source: SourceGeneric, fixture: "generic.json",
			headers: map[string]string{testAuthorization: "Bearer " + testToken},
			want:    v1alpha1.TriggerSourceAPI, target: "orders-data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			receiver, fakeClient, _ := testReceiver(t)
			recorder := deliver(t, receiver, test.source, fixture(t, test.fixture), test.headers)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200", recorder.Code, recorder.Body.String())
			}
			items := pages(t, fakeClient)
			if len(items) != 1 {
				t.Fatalf("Pages = %d, want 1", len(items))
			}
			opened := items[0]
			if opened.Spec.Target.Name != test.target {
				t.Errorf("Target = %q, want %q", opened.Spec.Target.Name, test.target)
			}
			if opened.Spec.Trigger.Source != test.want {
				t.Errorf("Trigger source = %q, want %q", opened.Spec.Trigger.Source, test.want)
			}
			if opened.Status.Subject == nil || opened.Status.Subject.Resolution == "" {
				t.Errorf("status = %+v, want a recorded subject resolution", opened.Status)
			}
			if len(opened.Status.Alerts) != 1 {
				t.Errorf("alerts = %d, want 1", len(opened.Status.Alerts))
			}
			if opened.Status.TriggerCount != 1 {
				t.Errorf("trigger count = %d, want 1", opened.Status.TriggerCount)
			}
		})
	}
}

func TestAnAlertJoinsThePageOfTheSameSubject(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	deployment := &appsv1.Deployment{}
	if err := fakeClient.Get(t.Context(),
		types.NamespacedName{Namespace: testShop, Name: testCheckout}, deployment); err != nil {
		t.Fatal(err)
	}
	watcherPage := &v1alpha1.Page{
		Name: "checkout-crashloop", Namespace: testNamespace,
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: testShop,
				Name: testCheckout, UID: string(deployment.UID),
			},
			Fingerprint: v1alpha1.Fingerprint(v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindDeployment, Namespace: testShop, Name: testCheckout,
				UID: string(deployment.UID),
			}, "CrashLoopBackOff"),
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent, Reason: "CrashLoopBackOff"},
		},
	}
	if err := fakeClient.Create(t.Context(), watcherPage); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Status.Update(t.Context(), client.ObjectKeyFromObject(watcherPage),
		func(latest *v1alpha1.Page) error {
			latest.Status.Phase = v1alpha1.PhaseInvestigating
			latest.Status.TriggerCount = 1
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	recorder := deliver(t, receiver, SourceAlertmanager, fixture(t, alertmanagerFixture),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", recorder.Code, recorder.Body.String())
	}
	items := pages(t, fakeClient)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want the watcher's Page only", len(items))
	}
	joined := items[0]
	if joined.Name != "checkout-crashloop" {
		t.Errorf("Page = %q, want the watcher's Page", joined.Name)
	}
	if joined.Status.TriggerCount != 2 {
		t.Errorf("trigger count = %d, want 2", joined.Status.TriggerCount)
	}
	if len(joined.Status.Alerts) != 1 {
		t.Fatalf("alerts = %d, want the alert recorded", len(joined.Status.Alerts))
	}
	if joined.Status.Alerts[0].State != v1alpha1.AlertStateFiring {
		t.Errorf("alert state = %q, want Firing", joined.Status.Alerts[0].State)
	}
	if joined.Status.Severity != v1alpha1.SeverityCritical {
		t.Errorf("severity = %q, want critical", joined.Status.Severity)
	}
}

func TestAResolvedAlertClosesOnlyItsTrigger(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	recorder := deliver(t, receiver, SourceAlertmanager, fixture(t, alertmanagerFixture),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusOK {
		t.Fatalf("firing status = %d", recorder.Code)
	}
	firing := pages(t, fakeClient)[0]
	resolved := strings.Replace(string(fixture(t, alertmanagerFixture)), `"status": "firing"`, `"status": "resolved"`, 1)
	resolved = strings.Replace(resolved, `"status": "firing"`, `"status": "resolved"`, 1)
	recorder = deliver(t, receiver, SourceAlertmanager, []byte(resolved),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusOK {
		t.Fatalf("resolved status = %d (%s), want 200", recorder.Code, recorder.Body.String())
	}
	items := pages(t, fakeClient)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want the same Page", len(items))
	}
	closed := items[0]
	if closed.Name != firing.Name {
		t.Errorf("Page = %q, want %q", closed.Name, firing.Name)
	}
	if len(closed.Status.Alerts) != 1 || closed.Status.Alerts[0].State != v1alpha1.AlertStateResolved {
		t.Errorf("alerts = %+v, want one resolved alert", closed.Status.Alerts)
	}
	if closed.Status.Phase != "" && !closed.Status.Phase.IsActive() {
		t.Errorf("phase = %q, want the Page to stay open", closed.Status.Phase)
	}
}

func TestAResolvedAlertWithNoActivePageIsIgnored(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	resolved := strings.Replace(string(fixture(t, alertmanagerFixture)), `"status": "firing"`, `"status": "resolved"`, 1)
	resolved = strings.Replace(resolved, `"status": "firing"`, `"status": "resolved"`, 1)
	recorder := deliver(t, receiver, SourceAlertmanager, []byte(resolved),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	answer := decodeResponse(t, recorder)
	if len(answer.Ignored) != 1 || answer.Ignored[0].Reason != "no-active-page" {
		t.Errorf("answer = %+v, want a no-active-page reason", answer)
	}
	if items := pages(t, fakeClient); len(items) != 0 {
		t.Errorf("Pages = %d, want none", len(items))
	}
}

func TestAnAlertThatNamesNoSubjectIsIgnoredWithAReason(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	body := `{"status":"firing","alerts":[{"status":"firing",` +
		`"labels":{"alertname":"Noisy","namespace":"shop","pagerless.dev/watch":"true"},` +
		`"annotations":{},"fingerprint":"noisy"}]}`
	recorder := deliver(t, receiver, SourceAlertmanager, []byte(body),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", recorder.Code)
	}
	answer := decodeResponse(t, recorder)
	if len(answer.Ignored) != 1 || answer.Ignored[0].Reason == "" {
		t.Errorf("answer = %+v, want a per-alert reason", answer)
	}
	if items := pages(t, fakeClient); len(items) != 0 {
		t.Errorf("Pages = %d, want none", len(items))
	}
}

func TestASenderRetryIsIdempotent(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, deliveries := testReceiver(t)
	for range 3 {
		recorder := deliver(t, receiver, SourceAlertmanager, fixture(t, alertmanagerFixture),
			map[string]string{testAuthorization: "Bearer " + testToken})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", recorder.Code, recorder.Body.String())
		}
	}
	items := pages(t, fakeClient)
	if len(items) != 1 {
		t.Fatalf("Pages = %d, want 1", len(items))
	}
	if items[0].Status.TriggerCount != 1 {
		t.Errorf("trigger count = %d, want 1", items[0].Status.TriggerCount)
	}
	if len(deliveries.records) != 1 {
		t.Errorf("deliveries = %d, want 1", len(deliveries.records))
	}
}

func TestAuthenticationAndTransportErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		build   func(*testing.T) *http.Request
		status  int
		options func(*Receiver)
	}{
		{
			name: "a wrong token reveals nothing",
			build: func(t *testing.T) *http.Request {
				return requestWith(t, SourceAlertmanager, fixture(t, alertmanagerFixture),
					map[string]string{testAuthorization: "Bearer wrong"})
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "a missing token",
			build: func(t *testing.T) *http.Request {
				return requestWith(t, SourceAlertmanager, fixture(t, alertmanagerFixture), nil)
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "an unconfigured source",
			options: func(r *Receiver) {
				r.DatadogToken = nil
			},
			build: func(t *testing.T) *http.Request {
				return requestWith(t, SourceDatadog, fixture(t, "datadog.json"),
					map[string]string{testAuthorization: "Bearer " + testToken})
			},
			status: http.StatusForbidden,
		},
		{
			name: "a wrong content type",
			build: func(t *testing.T) *http.Request {
				request := requestWith(t, SourceGeneric, fixture(t, "generic.json"), nil)
				request.Header.Set("Content-Type", "text/plain")
				return request
			},
			status: http.StatusUnsupportedMediaType,
		},
		{
			name: "a method other than POST",
			build: func(t *testing.T) *http.Request {
				request := requestWith(t, SourceGeneric, nil, nil)
				request.Method = http.MethodGet
				return request
			},
			status: http.StatusMethodNotAllowed,
		},
		{
			name: "an oversized body",
			build: func(t *testing.T) *http.Request {
				body := bytes.Repeat([]byte("a"), MaxBodyBytes+1)
				return requestWith(t, SourceGeneric, body, nil)
			},
			status: http.StatusRequestEntityTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := []func(*Receiver){}
			if test.options != nil {
				options = append(options, test.options)
			}
			receiver, fakeClient, _ := testReceiver(t, options...)
			recorder := httptest.NewRecorder()
			receiver.ServeHTTP(recorder, test.build(t))
			if recorder.Code != test.status {
				t.Fatalf("status = %d (%s), want %d", recorder.Code, recorder.Body.String(), test.status)
			}
			if test.status == http.StatusUnauthorized {
				for _, leaked := range []string{testToken, testGrafana} {
					if strings.Contains(recorder.Body.String(), leaked) {
						t.Errorf("the 401 body leaks the secret %q", leaked)
					}
				}
			}
			if items := pages(t, fakeClient); len(items) != 0 {
				t.Errorf("a refused request created %d Pages", len(items))
			}
		})
	}
}

// requestWith builds a POST request for a payload.
func requestWith(t *testing.T, source Source, body []byte, headers map[string]string) *http.Request {
	t.Helper()
	var request *http.Request
	if body == nil {
		request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, Path, http.NoBody)
	} else {
		request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, Path, bytes.NewReader(body))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SourceHeader, string(source))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}

func TestPerSourceRateLimitRunsBeforeDecoding(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t, func(r *Receiver) {
		r.RequestsPerSecond = 1
		r.Burst = 1
	})
	first := deliver(t, receiver, SourceGeneric, fixture(t, "generic.json"),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	second := deliver(t, receiver, SourceGeneric, fixture(t, "generic.json"),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", second.Code)
	}
	other := deliver(t, receiver, SourceAlertmanager, fixture(t, alertmanagerFixture),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if other.Code != http.StatusOK {
		t.Errorf("another source status = %d, want its own limit", other.Code)
	}
	if items := pages(t, fakeClient); len(items) != 1 && len(items) != 2 {
		t.Errorf("Pages = %d, want the accepted deliveries only", len(items))
	}
}

func TestTheGrafanaSignatureIsOverTheExactBody(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	body := fixture(t, "grafana.json")
	mac := hmacSHA256([]byte(testGrafana), body)
	recorder := deliver(t, receiver, SourceGrafana, body,
		map[string]string{testSignatureHeader: "sha256=" + hex.EncodeToString(mac)})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", recorder.Code, recorder.Body.String())
	}
	if items := pages(t, fakeClient); len(items) != 1 {
		t.Fatalf("Pages = %d, want 1", len(items))
	}

	other, otherClient, _ := testReceiver(t)
	tampered := append([]byte{}, body...)
	tampered[len(tampered)-2] = ' '
	recorder = deliver(t, other, SourceGrafana, tampered,
		map[string]string{testSignatureHeader: "sha256=" + hex.EncodeToString(mac)})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tampered status = %d, want 401", recorder.Code)
	}
	if items := pages(t, otherClient); len(items) != 0 {
		t.Errorf("a tampered delivery created %d Pages", len(items))
	}
}

func TestSubscriptionConfirmationIsReportedNotConfirmed(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t)
	recorder := deliver(t, receiver, SourceSNS, fixture(t, "sns-subscription.json"), nil)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", recorder.Code, recorder.Body.String())
	}
	answer := decodeResponse(t, recorder)
	if len(answer.Ignored) != 1 || answer.Ignored[0].Reason != "subscription-confirmation-not-accepted" {
		t.Errorf("answer = %+v, want the subscription refusal", answer)
	}
	if items := pages(t, fakeClient); len(items) != 0 {
		t.Errorf("a subscription confirmation created %d Pages", len(items))
	}
}

func TestThePerRequestAlertLimitIsReportedNotSilentlyDropped(t *testing.T) {
	t.Parallel()
	receiver, fakeClient, _ := testReceiver(t, func(r *Receiver) { r.MaxAlerts = 1 })
	body := `{"alerts":[
		{"status":"firing","name":"First","labels":{"namespace":"shop","deployment":"checkout","pagerless.dev/watch":"true"}},
		{"status":"firing","name":"Second","labels":{"namespace":"shop","deployment":"checkout","pagerless.dev/watch":"true"}}
	]}`
	recorder := deliver(t, receiver, SourceGeneric, []byte(body),
		map[string]string{testAuthorization: "Bearer " + testToken})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", recorder.Code, recorder.Body.String())
	}
	answer := decodeResponse(t, recorder)
	if answer.Applied != 1 || len(answer.Ignored) != 1 {
		t.Fatalf("answer = %+v, want one applied and one ignored alert", answer)
	}
	if answer.Ignored[0].Index != 1 || answer.Ignored[0].Reason != "per-request-alert-limit" {
		t.Errorf("ignored = %+v, want the second alert reported", answer.Ignored)
	}
	if items := pages(t, fakeClient); len(items) != 1 {
		t.Errorf("Pages = %d, want only the decoded alert", len(items))
	}
}

package webhooks

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// fixture reads one payload fixture.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

// token returns a fixed token source.
func token(value string) secret {
	return func() (string, error) { return value, nil }
}

// header builds one request header.
func header(values map[string]string) http.Header {
	built := make(http.Header)
	for key, value := range values {
		built.Set(key, value)
	}
	return built
}

// decodeFixture decodes one fixture with the authentication the dialect needs.
func decodeFixture(
	t *testing.T, source Source, name string, auth authenticator, headers map[string]string,
) ([]Alert, error) {
	t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	alerts, _, err := decode(decodeRequest{
		ctx: t.Context(), source: source, body: fixture(t, name), header: header(headers), auth: auth,
	})
	return alerts, err
}

func TestDecodeAlertmanager(t *testing.T) {
	t.Parallel()
	alerts, err := decodeFixture(t, SourceAlertmanager, alertmanagerFixture,
		authenticator{tokens: map[Source]secret{SourceAlertmanager: token("secret")}},
		map[string]string{testAuthorization: "Bearer secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
	alert := alerts[0]
	if alert.Source != SourceAlertmanager || alert.Status != StatusFiring {
		t.Errorf("alert = %+v, want a firing Alertmanager alert", alert)
	}
	if alert.Name != "OOMKilled" || alert.Fingerprint != "6f1a4f0c2d3e4b5a" {
		t.Errorf("identity = %q/%q, want OOMKilled/6f1a4f0c2d3e4b5a", alert.Name, alert.Fingerprint)
	}
	if alert.Severity != v1alpha1.SeverityCritical {
		t.Errorf("severity = %q, want critical", alert.Severity)
	}
	if alert.Labels["deployment"] != testCheckout || alert.Message != "checkout is OOMKilling" {
		t.Errorf("alert lost its labels or message: %+v", alert)
	}
}

func TestDecodeGrafanaRequiresASignature(t *testing.T) {
	t.Parallel()
	body := fixture(t, "grafana.json")
	mac := hmacSHA256([]byte("grafana-secret"), body)
	auth := authenticator{grafanaSecret: token("grafana-secret")}

	signed, _, err := decode(decodeRequest{
		ctx: t.Context(), source: SourceGrafana, body: body,
		header: header(map[string]string{testSignatureHeader: "sha256=" + hex.EncodeToString(mac)}),
		auth:   auth,
	})
	if err != nil {
		t.Fatalf("signed delivery: %v", err)
	}
	if len(signed) != 1 || signed[0].Labels["statefulset"] != "orders" {
		t.Fatalf("signed alerts = %+v, want the orders alert", signed)
	}

	if _, _, err := decode(decodeRequest{
		ctx: t.Context(), source: SourceGrafana, body: body, header: http.Header{}, auth: auth,
	}); ClientStatus(err) != http.StatusUnauthorized {
		t.Errorf("unsigned delivery error = %v, want 401", err)
	}
	if _, _, err := decode(decodeRequest{
		ctx: t.Context(), source: SourceGrafana, body: body,
		header: header(map[string]string{testSignatureHeader: "sha256=" + hex.EncodeToString([]byte("wrong"))}),
		auth:   auth,
	}); ClientStatus(err) != http.StatusUnauthorized {
		t.Errorf("wrong signature error = %v, want 401", err)
	}
}

func TestDecodeDatadog(t *testing.T) {
	t.Parallel()
	alerts, err := decodeFixture(t, SourceDatadog, "datadog.json",
		authenticator{tokens: map[Source]secret{SourceDatadog: token("datadog")}},
		map[string]string{testAuthorization: "Bearer datadog"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
	alert := alerts[0]
	if alert.Labels["namespace"] != "shop" || alert.Labels["deployment"] != testCheckout {
		t.Errorf("labels = %v, want the Datadog tags mapped to subject labels", alert.Labels)
	}
	if alert.Severity != v1alpha1.SeverityCritical || alert.Status != StatusFiring {
		t.Errorf("alert = %+v, want a critical firing alert", alert)
	}
	if alert.Fingerprint != "dd-alert-7781" {
		t.Errorf("fingerprint = %q, want the Datadog alert id", alert.Fingerprint)
	}
}

func TestDecodeSNSAndCloudWatch(t *testing.T) {
	t.Parallel()
	auth := authenticator{snsTopics: []string{testTopicARN}}
	alerts, err := decodeFixture(t, SourceSNS, "sns-notification.json", auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
	alert := alerts[0]
	if alert.Name != "checkout-unavailable" || alert.Status != StatusFiring {
		t.Errorf("alert = %+v, want a firing checkout alarm", alert)
	}
	if alert.Labels["namespace"] != "shop" || alert.Labels["deployment"] != testCheckout {
		t.Errorf("labels = %v, want the alarm dimensions mapped to subject labels", alert.Labels)
	}
	if alert.Labels["pagerless.dev/watch"] != "true" {
		t.Errorf("labels = %v, want the opt-in dimension preserved", alert.Labels)
	}
}

func TestDecodeSNSRefusesAnUnknownTopicAndNeverConfirmsASubscription(t *testing.T) {
	t.Parallel()
	auth := authenticator{snsTopics: []string{"arn:aws:sns:us-east-1:123456789012:another-topic"}}
	_, err := decodeFixture(t, SourceSNS, "sns-notification.json", auth, nil)
	if ClientStatus(err) != http.StatusForbidden {
		t.Errorf("unknown topic error = %v, want 403", err)
	}
	allowed := authenticator{snsTopics: []string{testTopicARN}}
	_, err = decodeFixture(t, SourceSNS, "sns-subscription.json", allowed, nil)
	var ignore *Ignore
	if !errors.As(err, &ignore) {
		t.Fatalf("subscription confirmation error = %v, want an Ignore", err)
	}
	if ignore.Reason != "subscription-confirmation-not-accepted" {
		t.Errorf("reason = %q, want subscription-confirmation-not-accepted", ignore.Reason)
	}
}

func TestDecodeGeneric(t *testing.T) {
	t.Parallel()
	alerts, err := decodeFixture(t, SourceGeneric, "generic.json",
		authenticator{tokens: map[Source]secret{SourceGeneric: token("generic")}},
		map[string]string{testAuthorization: "Bearer generic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(alerts))
	}
	if alerts[0].Name != "PVCAlmostFull" || alerts[0].Severity != v1alpha1.SeverityWarning {
		t.Errorf("alert = %+v, want the PVC warning", alerts[0])
	}
}

func TestDecodeRejectsMalformedAndUnrepresentablePayloads(t *testing.T) {
	t.Parallel()
	auth := authenticator{
		tokens: map[Source]secret{
			SourceAlertmanager: token("secret"), SourceDatadog: token("secret"),
			SourceGeneric: token("secret"),
		},
	}
	tests := []struct {
		name   string
		source Source
		body   string
		status int
	}{
		{name: "alertmanager is not JSON", source: SourceAlertmanager, body: "{", status: http.StatusBadRequest},
		{name: "alertmanager has no alerts", source: SourceAlertmanager, body: `{"alerts":[]}`,
			status: http.StatusBadRequest},
		{name: "datadog has no identity", source: SourceDatadog, body: `{"tags":"x:y"}`,
			status: http.StatusBadRequest},
		{name: "generic has no alert", source: SourceGeneric, body: `{"labels":{}}`,
			status: http.StatusBadRequest},
		{name: "generic is not JSON", source: SourceGeneric, body: `not json`, status: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := decode(decodeRequest{
				ctx: t.Context(), source: test.source, body: []byte(test.body),
				header: header(map[string]string{testAuthorization: "Bearer secret"}), auth: auth,
			})
			if got := ClientStatus(err); got != test.status {
				t.Errorf("status = %d (%v), want %d", got, err, test.status)
			}
		})
	}
}

func TestDetectSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		headers map[string]string
		fixture string
		want    Source
	}{
		{name: "explicit header", headers: map[string]string{SourceHeader: "generic"}, fixture: alertmanagerFixture,
			want: SourceGeneric},
		{name: "sns header", headers: map[string]string{"X-Amz-Sns-Message-Type": "Notification"},
			fixture: "sns-notification.json", want: SourceSNS},
		{name: "grafana signature", headers: map[string]string{testSignatureHeader: "sha256=00"},
			fixture: "grafana.json", want: SourceGrafana},
		{name: "datadog shape", headers: nil, fixture: "datadog.json", want: SourceDatadog},
		{name: "generic shape", headers: nil, fixture: "generic.json", want: SourceGeneric},
		{name: "alertmanager shape", headers: nil, fixture: alertmanagerFixture, want: SourceAlertmanager},
		{name: "unparseable defaults to generic", headers: nil, fixture: "none", want: SourceGeneric},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := []byte("not json")
			if test.fixture != "none" {
				body = fixture(t, test.fixture)
			}
			if got := detectedSource(header(test.headers), body); got != test.want {
				t.Errorf("source = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSNSSignatureVerification(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certURL := "https://sns.us-east-1.amazonaws.com/pagerless-test.pem"
	setCertificateForTest(certURL, &key.PublicKey)
	envelope := snsEnvelope{
		Type: snsNotificationType, MessageID: "signed-1", TopicARN: testTopicARN,
		Message: `{"AlarmName":"checkout-unavailable","NewStateValue":"ALARM","Trigger":{"Dimensions":[` +
			`{"name":"Namespace","value":"shop"},{"name":"Deployment","value":"checkout"},` +
			`{"name":"pagerless.dev/watch","value":"true"}]}}`,
		Timestamp: "2026-11-16T12:02:01.000Z", SignatureVersion: "2", SigningCertURL: certURL,
	}
	digest := sha256.Sum256([]byte(snsStringToSign(envelope)))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	envelope.Signature = base64.StdEncoding.EncodeToString(signature)
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	auth := authenticator{snsTopics: []string{testTopicARN}, verifySNS: true}
	alerts, _, err := decode(decodeRequest{ctx: t.Context(), source: SourceSNS, body: body, auth: auth})
	if err != nil {
		t.Fatalf("a verified delivery: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Name != "checkout-unavailable" {
		t.Fatalf("alerts = %+v, want the CloudWatch alarm", alerts)
	}

	tampered := bytes.Replace(body, []byte("checkout-unavailable"), []byte("checkout-degraded"), 1)
	_, _, err = decode(decodeRequest{ctx: t.Context(), source: SourceSNS, body: tampered, auth: auth})
	if ClientStatus(err) != http.StatusUnauthorized {
		t.Errorf("tampered delivery error = %v, want 401", err)
	}
}

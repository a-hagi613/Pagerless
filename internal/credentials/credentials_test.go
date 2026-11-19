package credentials_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

const (
	testNamespace = "pagerless"
	testSecret    = "pagerless-credentials"
	testAPIKey    = "sk-test"
)

// newStore builds a store over a fake client that holds secret.
func newStore(t *testing.T, secret *corev1.Secret, external string) (*credentials.Store, client.Client) {
	t.Helper()
	return newStoreWith(t, secret, external, false)
}

// newStoreWith builds a store and chooses whether an externally managed Secret
// is read through the API, as the CLI does, or from a mounted volume, as the
// server does.
func newStoreWith(
	t *testing.T, secret *corev1.Secret, external string, readThroughAPI bool,
) (*credentials.Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if secret != nil {
		builder = builder.WithObjects(secret)
	}
	cl := builder.Build()
	store, err := credentials.New(credentials.Config{
		Client:          cl,
		Namespace:       testNamespace,
		SecretName:      testSecret,
		ExternalManager: external,
		ReadThroughAPI:  readThroughAPI,
		Now:             func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, cl
}

// emptySecret is the chart's pre-created Secret: every key present and empty.
func emptySecret() *corev1.Secret {
	data := map[string][]byte{}
	for _, key := range credentials.Keys() {
		data[key] = []byte("")
	}
	meta := metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret}
	return &corev1.Secret{ObjectMeta: meta, Data: data}
}

func TestWriteMergesOnlyTheSubmittedKeys(t *testing.T) {
	t.Parallel()
	secret := emptySecret()
	secret.Data[credentials.KeyAIBaseURL] = []byte("https://models.example/v1")
	store, cl := newStore(t, secret, "")

	view, err := store.Write(context.Background(), map[string]string{
		credentials.KeyAIAPIKey: "sk-test-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !view.Fields[credentials.KeyAIAPIKey].Configured {
		t.Error("the submitted key is not reported as configured")
	}
	if view.Fields[credentials.KeyAIAPIKey].Fingerprint == "" {
		t.Error("the submitted key carries no fingerprint")
	}
	if view.Fields[credentials.KeyAIAPIKey].UpdatedAt == nil {
		t.Error("the submitted key carries no last-updated time")
	}

	var stored corev1.Secret
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &stored); err != nil {
		t.Fatal(err)
	}
	if got := string(stored.Data[credentials.KeyAIAPIKey]); got != "sk-test-value" {
		t.Errorf("AI_API_KEY = %q, want the submitted value", got)
	}
	if got := string(stored.Data[credentials.KeyAIBaseURL]); got != "https://models.example/v1" {
		t.Errorf("AI_BASE_URL = %q, want the unsubmitted value preserved", got)
	}
	for _, key := range []string{credentials.KeyGitHubToken, credentials.KeyGitHubRepo, credentials.KeyAIModel} {
		if _, ok := stored.Data[key]; !ok {
			t.Errorf("key %q disappeared from the Secret", key)
		}
	}
}

func TestViewNeverReturnsAValue(t *testing.T) {
	t.Parallel()
	secret := emptySecret()
	secret.Data[credentials.KeyAIAPIKey] = []byte("super-secret-value")
	secret.Data[credentials.KeyAIBaseURL] = []byte("https://models.example/v1")
	store, _ := newStore(t, secret, "")

	view, err := store.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Fields[credentials.KeyAIAPIKey].Fingerprint == "super-secret-value" {
		t.Fatal("the view returned the stored value as a fingerprint")
	}
	if !strings.HasPrefix(view.Fields[credentials.KeyAIAPIKey].Fingerprint, "sha256:") {
		t.Errorf("fingerprint = %q, want a sha256 prefix", view.Fields[credentials.KeyAIAPIKey].Fingerprint)
	}
	if len(view.Fields[credentials.KeyAIAPIKey].Fingerprint) != len("sha256:")+8 {
		t.Errorf("fingerprint = %q, want eight hex characters", view.Fields[credentials.KeyAIAPIKey].Fingerprint)
	}
}

func TestAnExternalSecretRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	store, cl := newStore(t, emptySecret(), "team-credentials")

	_, err := store.Write(context.Background(), map[string]string{credentials.KeyAIAPIKey: testAPIKey})
	var external *credentials.ExternalError
	if !errors.As(err, &external) {
		t.Fatalf("error = %v, want an ExternalError", err)
	}
	if external.Manager != "team-credentials" {
		t.Errorf("manager = %q, want team-credentials", external.Manager)
	}
	var stored corev1.Secret
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &stored)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(stored.Data[credentials.KeyAIAPIKey]); got != "" {
		t.Errorf("AI_API_KEY = %q, want no write to an externally managed Secret", got)
	}
}

func TestAMissingSecretIsAMisconfigurationNotACreate(t *testing.T) {
	t.Parallel()
	store, cl := newStore(t, nil, "")

	_, err := store.Write(context.Background(), map[string]string{credentials.KeyAIAPIKey: testAPIKey})
	if _, ok := errors.AsType[*credentials.MissingError](err); !ok {
		t.Fatalf("error = %v, want a MissingError", err)
	}
	var secret corev1.Secret
	err = cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &secret)
	if err == nil {
		t.Fatal("the store created the missing Secret")
	}
}

func TestUnknownKeysAreRefused(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t, emptySecret(), "")
	for _, key := range []string{"API_KEY", "ai_api_key", "CONNECTOR_ TOKEN", "GITHUB_TOKEN "} {
		if _, err := store.Write(context.Background(), map[string]string{key: "value"}); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}
}

func TestConnectorTokenKeyUsesUpperSnake(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"loki":          "CONNECTOR_LOKI_TOKEN",
		"cloud-watch":   "CONNECTOR_CLOUD_WATCH_TOKEN",
		"prometheus-01": "CONNECTOR_PROMETHEUS_01_TOKEN",
	} {
		if got := credentials.ConnectorTokenKey(name); got != want {
			t.Errorf("ConnectorTokenKey(%q) = %q, want %q", name, got, want)
		}
		if !credentials.SecretKey(want) {
			t.Errorf("SecretKey(%q) = false, want true", want)
		}
	}
}

func TestClearEmptiesAKeyAndKeepsItPresent(t *testing.T) {
	t.Parallel()
	secret := emptySecret()
	secret.Data[credentials.KeyGitHubToken] = []byte("ghp_secret")
	store, cl := newStore(t, secret, "")

	if _, err := store.Clear(context.Background(), []string{credentials.KeyGitHubToken}); err != nil {
		t.Fatal(err)
	}
	var stored corev1.Secret
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &stored); err != nil {
		t.Fatal(err)
	}
	value, ok := stored.Data[credentials.KeyGitHubToken]
	if !ok {
		t.Fatal("the cleared key disappeared; the mounted volume needs the file")
	}
	if len(value) != 0 {
		t.Errorf("cleared value = %q, want empty", value)
	}
}

// TestReadThroughAPIViewsAnExternalSecret keeps the CLI able to show a masked,
// read-only view of a Secret a secret manager owns without a mounted volume.
func TestReadThroughAPIViewsAnExternalSecret(t *testing.T) {
	t.Parallel()
	secret := emptySecret()
	secret.Data[credentials.KeyAIBaseURL] = []byte("https://models.example/v1")
	store, _ := newStoreWith(t, secret, "team-credentials", true)

	if store.Hermetic() {
		t.Error("ReadThroughAPI must not read the mounted volume")
	}
	view, err := store.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ManagedBy != "external" || view.Manager != "team-credentials" || view.WriteCredentials {
		t.Errorf("view = %+v, want an external, read-only view", view)
	}
	if !view.Fields[credentials.KeyAIBaseURL].Configured {
		t.Error("the API-read key is not reported as configured")
	}
	if _, err := store.Write(context.Background(),
		map[string]string{credentials.KeyAIAPIKey: testAPIKey}); !errors.As(err, new(*credentials.ExternalError)) {
		t.Errorf("write error = %v, want an ExternalError", err)
	}
}

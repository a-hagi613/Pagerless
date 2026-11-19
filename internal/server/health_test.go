package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

// repositorySubsystem is the readiness name of the Target Repository dependency.
const repositorySubsystem = "repository"

// credentialStore returns a store over a Secret that carries the given values.
func credentialStore(t *testing.T, values map[string]string) *credentials.Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for _, key := range credentials.Keys() {
		data[key] = []byte(values[key])
	}
	meta := metav1.ObjectMeta{Namespace: "pagerless", Name: "pagerless-credentials"}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{ObjectMeta: meta, Data: data}).Build()
	store, err := credentials.New(credentials.Config{
		Client: cl, Namespace: "pagerless", SecretName: "pagerless-credentials",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestReadinessReportsEachUnfinishedSubsystem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		store  *credentials.Store
		wanted []string
	}{
		{
			name:   "a fresh install has no AI endpoint and no repository",
			store:  credentialStore(t, nil),
			wanted: []string{"ai", repositorySubsystem},
		},
		{
			name: "an AI endpoint without a repository leaves the repository degraded",
			store: credentialStore(t, map[string]string{
				credentials.KeyAIBaseURL: "https://models.example/v1",
				credentials.KeyAIAPIKey:  "sk-test",
			}),
			wanted: []string{repositorySubsystem},
		},
		{
			name: "a full configuration reports nothing degraded",
			store: credentialStore(t, map[string]string{
				credentials.KeyAIBaseURL:   "https://models.example/v1",
				credentials.KeyAIAPIKey:    "sk-test",
				credentials.KeyGitHubRepo:  "acme/shop",
				credentials.KeyGitHubToken: "ghp-test",
			}),
			wanted: []string{},
		},
		{
			name:   "no credentials store leaves both degraded",
			store:  nil,
			wanted: []string{"ai", repositorySubsystem},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := readiness{Credentials: tc.store}
			got := probe.degraded(t.Context())
			if got == nil {
				got = []string{}
			}
			if !slices.Equal(got, tc.wanted) {
				t.Errorf("degraded = %v, want %v", got, tc.wanted)
			}
		})
	}
}

func TestReadyzStaysReadyWhileReportingDegradedSubsystems(t *testing.T) {
	t.Parallel()
	probe := readiness{Credentials: credentialStore(t, nil)}
	server := httptest.NewServer(probe.handler())
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200 so the pod stays in the Service", response.StatusCode)
	}
	var body struct {
		Ready    bool     `json:"ready"`
		Degraded []string `json:"degraded"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Error("readyz reported not ready for an unfinished installation")
	}
	if !slices.Equal(body.Degraded, []string{"ai", repositorySubsystem}) {
		t.Errorf("degraded = %v, want the unfinished subsystems", body.Degraded)
	}

	health, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", health.StatusCode)
	}
}

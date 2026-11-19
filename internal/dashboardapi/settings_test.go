package dashboardapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
)

const (
	testNamespace = "pagerless"
	testSecret    = "pagerless-credentials"
	testCSRF      = "csrf-token"
)

// fakeAuth signs in one user when the request carries its cookie.
type fakeAuth struct{}

func (fakeAuth) User(r *http.Request) *dashboardapi.User {
	if _, err := r.Cookie("session"); err != nil {
		return nil
	}
	return &dashboardapi.User{Subject: "admin", Username: "admin", Approver: true, CSRF: testCSRF, Mode: "token"}
}

func (fakeAuth) ValidateCSRF(r *http.Request, user *dashboardapi.User) bool {
	return r.Header.Get("X-CSRF-Token") == user.CSRF
}

func (fakeAuth) Login() http.Handler  { return http.NotFoundHandler() }
func (fakeAuth) Logout() http.Handler { return http.NotFoundHandler() }
func (fakeAuth) Mode() string         { return "token" }

// newHandler builds the API over a fake client holding the chart's empty Secret.
func newHandler(t *testing.T, external string) (*dashboardapi.Handler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{}
	for _, key := range credentials.Keys() {
		data[key] = []byte("")
	}
	meta := metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret}
	secret := &corev1.Secret{ObjectMeta: meta, Data: data}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	mountPath := ""
	if external != "" {
		// An externally managed Secret is read from the mounted volume, which the
		// kubelet projects from the Secret.
		mountPath = t.TempDir()
		for _, key := range credentials.Keys() {
			if err := os.WriteFile(filepath.Join(mountPath, key), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := credentials.New(credentials.Config{
		Client:          cl,
		Namespace:       testNamespace,
		SecretName:      testSecret,
		ExternalManager: external,
		MountPath:       mountPath,
		Now:             func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return dashboardapi.New(dashboardapi.Config{
		Namespace:   testNamespace,
		Reader:      cl,
		Writer:      cl,
		Auth:        fakeAuth{},
		Credentials: store,
		Broker:      dashboardapi.NewBroker(),
		Version:     "2.0.0",
	}), cl
}

// request performs one API call with a session and its CSRF token.
func request(t *testing.T, handler http.Handler, method, body string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	return requestPath(t, handler, method, "/api/settings", body)
}

// requestPath performs one API call against an explicit path.
func requestPath(t *testing.T, handler http.Handler, method, path, body string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: "session", Value: "signed"})
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", testCSRF)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, rec.Body.Bytes()
}

func TestSettingsMasksEveryCredential(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(t, "")
	rec, body := request(t, handler, http.MethodPatch,
		`{"ai":{"apiKey":"sk-super-secret","baseUrl":"https://models.example/v1","model":"novalite"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/settings = %d: %s", rec.Code, body)
	}
	if strings.Contains(string(body), "sk-super-secret") {
		t.Fatalf("the answer echoed the submitted value:\n%s", body)
	}
	var answer struct {
		ManagedBy        string                       `json:"managedBy"`
		WriteCredentials bool                         `json:"writeCredentials"`
		Onboarding       bool                         `json:"onboarding"`
		Values           map[string]string            `json:"values"`
		Fields           map[string]credentials.Field `json:"fields"`
		AI               struct {
			BaseURL string `json:"baseUrl"`
		} `json:"ai"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.ManagedBy != "portal" || !answer.WriteCredentials {
		t.Errorf("managedBy = %q, writeCredentials = %v, want portal and true", answer.ManagedBy, answer.WriteCredentials)
	}
	if !answer.Onboarding {
		t.Error("onboarding is complete although no Target Repository is configured")
	}
	if answer.AI.BaseURL != "https://models.example/v1" {
		t.Errorf("ai.baseUrl = %q, want the non-secret value", answer.AI.BaseURL)
	}
	if !answer.Fields[credentials.KeyAIAPIKey].Configured {
		t.Error("AI_API_KEY is not reported as configured")
	}
}

func TestExternalSecretRefusesTheWriteWithACodeAndManager(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(t, "team-credentials")
	rec, body := request(t, handler, http.MethodPatch,
		`{"values":{"AI_API_KEY":"sk-super-secret"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH /api/settings = %d, want 409: %s", rec.Code, body)
	}
	var answer struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Manager string `json:"manager"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Error.Code != "settings_externally_managed" {
		t.Errorf("code = %q, want settings_externally_managed", answer.Error.Code)
	}
	if answer.Error.Manager != "team-credentials" {
		t.Errorf("manager = %q, want team-credentials", answer.Error.Manager)
	}
	if strings.Contains(string(body), "sk-super-secret") {
		t.Error("the refusal echoed the submitted value")
	}

	rec, body = request(t, handler, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d: %s", rec.Code, body)
	}
	if !strings.Contains(string(body), `"managedBy":"external"`) {
		t.Errorf("GET did not report external ownership:\n%s", body)
	}
}

func TestBothSurfacesWriteTheIdenticalKeySet(t *testing.T) {
	t.Parallel()
	apiHandler, apiClient := newHandler(t, "")

	// The Portal path: one grouped PATCH, exactly what the settings form posts.
	rec, body := request(t, apiHandler, http.MethodPatch,
		`{"ai":{"apiKey":"sk-1","baseUrl":"https://models.example/v1","model":"m"},
		  "repository":{"owner":"acme","repo":"shop","token":"ghp_1"},
		  "connectors":[{"name":"loki","token":"loki-token"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/settings = %d: %s", rec.Code, body)
	}

	// The CLI path: the same values written through the shared store.
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cliMeta := metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret}
	cliSecret := &corev1.Secret{ObjectMeta: cliMeta, Data: map[string][]byte{}}
	for _, key := range credentials.Keys() {
		cliSecret.Data[key] = []byte("")
	}
	cliClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cliSecret).Build()
	cliStore, err := credentials.New(credentials.Config{
		Client: cliClient, Namespace: testNamespace, SecretName: testSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliStore.Write(t.Context(), map[string]string{
		credentials.KeyAIAPIKey:               "sk-1",
		credentials.KeyAIBaseURL:              "https://models.example/v1",
		credentials.KeyAIModel:                "m",
		credentials.KeyGitHubToken:            "ghp_1",
		credentials.KeyGitHubRepo:             "acme/shop",
		credentials.ConnectorTokenKey("loki"): "loki-token",
	}); err != nil {
		t.Fatal(err)
	}

	var fromAPI, fromCLI corev1.Secret
	if err := apiClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &fromAPI); err != nil {
		t.Fatal(err)
	}
	if err := cliClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &fromCLI); err != nil {
		t.Fatal(err)
	}
	apiKeys := make([]string, 0, len(fromAPI.Data))
	for key := range fromAPI.Data {
		apiKeys = append(apiKeys, key)
	}
	cliKeys := make([]string, 0, len(fromCLI.Data))
	for key := range fromCLI.Data {
		cliKeys = append(cliKeys, key)
	}
	slices.Sort(apiKeys)
	slices.Sort(cliKeys)
	if !slices.Equal(apiKeys, cliKeys) {
		t.Errorf("key sets differ:\nAPI %v\nCLI %v", apiKeys, cliKeys)
	}
	if !slices.Contains(apiKeys, "CONNECTOR_LOKI_TOKEN") {
		t.Errorf("key set %v lacks the connector token key", apiKeys)
	}
}

func TestSettingsWritesRequireASessionAndCSRF(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(t, "")

	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"values":{"AI_API_KEY":"x"}}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("write without a session = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(`{"values":{"AI_API_KEY":"x"}}`))
	req.AddCookie(&http.Cookie{Name: "session", Value: "signed"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("write without a CSRF token = %d, want 403", rec.Code)
	}
}

func TestUnknownSettingKeyIsRefused(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(t, "")
	rec, body := request(t, handler, http.MethodPatch, `{"values":{"AI_PASSWORD":"x"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d, want 400: %s", rec.Code, body)
	}
	if !strings.Contains(string(body), "unknown credential key") {
		t.Errorf("error = %s, want it to name the unknown key", body)
	}
}

// TestTheOnboardingWizardConvergesOnTheCLIKeySet writes the same three sections the Portal's onboarding
// wizard writes, in the order an operator fills them in, and asserts the Secret ends with exactly the
// key set `kubectl page config set` writes in one pass.
func TestTheOnboardingWizardConvergesOnTheCLIKeySet(t *testing.T) {
	t.Parallel()
	apiHandler, apiClient := newHandler(t, "")

	steps := []string{
		`{"ai":{"baseUrl":"https://models.example/v1","model":"m","apiKey":"sk-1"}}`,
		`{"repository":{"owner":"acme","repo":"shop","token":"ghp_1"}}`,
		`{"connectors":[{"name":"loki","token":"loki-token"}]}`,
	}
	for _, step := range steps {
		rec, body := request(t, apiHandler, http.MethodPatch, step)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s = %d: %s", step, rec.Code, body)
		}
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cliSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret},
		Data:       map[string][]byte{},
	}
	for _, key := range credentials.Keys() {
		cliSecret.Data[key] = []byte("")
	}
	cliClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cliSecret).Build()
	cliStore, err := credentials.New(credentials.Config{
		Client: cliClient, Namespace: testNamespace, SecretName: testSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cliStore.Write(t.Context(), map[string]string{
		credentials.KeyAIBaseURL:              "https://models.example/v1",
		credentials.KeyAIModel:                "m",
		credentials.KeyAIAPIKey:               "sk-1",
		credentials.KeyGitHubRepo:             "acme/shop",
		credentials.KeyGitHubToken:            "ghp_1",
		credentials.ConnectorTokenKey("loki"): "loki-token",
	}); err != nil {
		t.Fatal(err)
	}

	var fromAPI, fromCLI corev1.Secret
	if err := apiClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &fromAPI); err != nil {
		t.Fatal(err)
	}
	if err := cliClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testSecret}, &fromCLI); err != nil {
		t.Fatal(err)
	}
	apiKeys, cliKeys := make([]string, 0, len(fromAPI.Data)), make([]string, 0, len(fromCLI.Data))
	for key := range fromAPI.Data {
		apiKeys = append(apiKeys, key)
	}
	for key := range fromCLI.Data {
		cliKeys = append(cliKeys, key)
	}
	slices.Sort(apiKeys)
	slices.Sort(cliKeys)
	if !slices.Equal(apiKeys, cliKeys) {
		t.Fatalf("the wizard's incremental writes produced a different key set:\nwizard %v\nCLI    %v", apiKeys, cliKeys)
	}
}

// TestSettingsTestProbesEachTarget exercises the test action with a real HTTP server standing in for the
// provider, so the answer reflects an actual request and not a hard-coded result.
func TestSettingsTestProbesEachTarget(t *testing.T) {
	t.Parallel()
	var answered []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answered = append(answered, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if r.Header.Get("Authorization") != "Bearer sk-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case strings.HasPrefix(r.URL.Path, "/repos/acme/shop"):
			if r.Header.Get("Authorization") != "Bearer ghp_1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()

	handler, _ := newHandler(t, "")
	rec, body := request(t, handler, http.MethodPatch, `{
		"ai":{"baseUrl":"`+provider.URL+`","model":"m","apiKey":"sk-1"},
		"repository":{"owner":"acme","repo":"shop","token":"ghp_1","apiUrl":"`+provider.URL+`"},
		"connectors":[{"name":"loki","token":"loki-token"}]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, body)
	}

	for _, test := range []struct {
		target string
		want   bool
	}{
		{"ai", true},
		{"github", true},
		{"connector:loki", true},
		{"connector:prometheus", false},
	} {
		rec, body := requestPath(t, handler, http.MethodPost, "/api/settings/test", `{"target":"`+test.target+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /api/settings/test %s = %d: %s", test.target, rec.Code, body)
		}
		var answer struct {
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatal(err)
		}
		if answer.OK != test.want {
			t.Errorf("%s: ok = %v, want %v (%s)", test.target, answer.OK, test.want, answer.Detail)
		}
	}
	if !slices.Contains(answered, "/models") || !slices.Contains(answered, "/repos/acme/shop") {
		t.Errorf("the probes reached %v, want the model list and the repository", answered)
	}
}

func TestSettingsTestRefusesAnUnknownTarget(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(t, "")
	rec, body := requestPath(t, handler, http.MethodPost, "/api/settings/test", `{"target":"cluster"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/settings/test = %d, want 400: %s", rec.Code, body)
	}
	if !strings.Contains(string(body), "settings_test_target") {
		t.Errorf("error = %s, want settings_test_target", body)
	}
}

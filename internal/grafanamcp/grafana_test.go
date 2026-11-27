package grafanamcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

func TestGrafanaDashboardAndDatasources(t *testing.T) {
	var seenAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenAuth = request.Header.Get("Authorization")
		switch request.URL.Path {
		case "/api/dashboards/uid/pagerless-target":
			_, _ = writer.Write([]byte(`{"dashboard":{"uid":"pagerless-target","title":"Pagerless Target metrics"}}`))
		case "/api/datasources":
			_, _ = writer.Write([]byte(`[{
				"name":"Prometheus","type":"prometheus",
				"secureJsonData":{"password":"secret"},
				"password":"secret","token":"secret"
			}]`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider, err := New(server.URL, func(context.Context) (string, error) {
		return "service-token", nil
	}, server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dashboard := toolByName(t, provider.Tools(), "grafana_dashboard")
	result, err := dashboard.Call(context.Background(), json.RawMessage(`{"uid":"pagerless-target"}`))
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if !strings.Contains(result, "Pagerless Target metrics") || seenAuth != "Bearer service-token" {
		t.Fatalf("dashboard result %q, auth %q", result, seenAuth)
	}
	datasources := toolByName(t, provider.Tools(), "grafana_datasources")
	clean, err := datasources.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("datasources: %v", err)
	}
	for _, secret := range []string{"secret", "secureJsonData", "password", "token"} {
		if strings.Contains(clean, secret) {
			t.Fatalf("the data source result still contains %q: %s", secret, clean)
		}
	}
}

func TestGrafanaRejectsAnInvalidUID(t *testing.T) {
	provider, err := New("http://grafana.example", nil, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dashboard := toolByName(t, provider.Tools(), "grafana_dashboard")
	if _, err := dashboard.Call(context.Background(), json.RawMessage(`{"uid":"../admin"}`)); err == nil {
		t.Fatal("an invalid dashboard UID was accepted")
	}
}

func toolByName(t *testing.T, tools []mcpserver.Tool, name string) mcpserver.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is missing", name)
	return mcpserver.Tool{}
}

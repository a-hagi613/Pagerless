package prometheusmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

func TestPrometheusToolsQueryAndAlerts(t *testing.T) {
	var seenAuth, seenQuery string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenAuth = request.Header.Get("Authorization")
		switch request.URL.Path {
		case "/api/v1/query":
			seenQuery = request.URL.Query().Get("query")
			_, _ = writer.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		case "/api/v1/alerts":
			_, _ = writer.Write([]byte(`{"status":"success","data":{"alerts":[{"labels":{"alertname":"ContainerOOMKilled"}}]}}`))
		case "/api/v1/targets":
			if request.URL.Query().Get("state") != "active" {
				t.Errorf("targets did not request the active state")
			}
			_, _ = writer.Write([]byte(`{"status":"success","data":{"activeTargets":[]}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	provider, err := New(server.URL, func(context.Context) (string, error) { return "read-token", nil }, server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	tools := provider.Tools()
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	query := toolByName(t, tools, "prometheus_query")
	result, err := query.Call(context.Background(), json.RawMessage(`{"query":"up == 0"}`))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(result, `"success"`) || seenQuery != "up == 0" {
		t.Fatalf("query result %q, seen query %q", result, seenQuery)
	}
	if seenAuth != "Bearer read-token" {
		t.Fatalf("authorization = %q", seenAuth)
	}
	alerts := toolByName(t, tools, "prometheus_alerts")
	if _, err := alerts.Call(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("alerts: %v", err)
	}
}

func TestPrometheusRejectsAnEmptyQueryAndAnErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "query failed", http.StatusBadRequest)
	}))
	defer server.Close()
	provider, err := New(server.URL, nil, server.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	query := toolByName(t, provider.Tools(), "prometheus_query")
	if _, err := query.Call(context.Background(), json.RawMessage(`{"query":""}`)); err == nil {
		t.Fatal("an empty query was accepted")
	}
	if _, err := query.Call(context.Background(), json.RawMessage(`{"query":"up"}`)); err == nil {
		t.Fatal("an HTTP error was accepted")
	}
}

func TestPrometheusRejectsANonHTTPURL(t *testing.T) {
	if _, err := New("prometheus:9090", nil, nil); err == nil {
		t.Fatal("a relative URL was accepted")
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

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testMCP(t *testing.T) *httptest.Server {
	t.Helper()
	server := &Server{
		Name:  "test",
		Token: func() (string, error) { return "token", nil },
		Tools: []Tool{{
			Name:        "echo",
			Description: "echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Call: func(_ context.Context, arguments json.RawMessage) (string, error) {
				return string(arguments), nil
			},
		}},
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func rpc(t *testing.T, server *httptest.Server, token string, body string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var decoded map[string]any
	if response.StatusCode != http.StatusAccepted {
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return response.StatusCode, decoded
}

func TestServerRequiresTheIntegrationToken(t *testing.T) {
	t.Parallel()
	server := testMCP(t)
	status, _ := rpc(t, server, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request answered %d", status)
	}
}

func TestServerHandshakeListAndCall(t *testing.T) {
	t.Parallel()
	server := testMCP(t)
	status, body := rpc(t, server, "token",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if status != http.StatusOK {
		t.Fatalf("initialize answered %d", status)
	}
	result := body["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("handshake %+v", result)
	}

	_, body = rpc(t, server, "token", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := body["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools %+v", tools)
	}

	status, body = rpc(t, server, "token",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`)
	if status != http.StatusOK {
		t.Fatalf("call answered %d", status)
	}
	content := body["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["text"] != `{"a":1}` {
		t.Fatalf("tool text %q", content["text"])
	}

	status, body = rpc(t, server, "token",
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing"}}`)
	if status != http.StatusOK {
		t.Fatalf("unknown tool answered %d", status)
	}
	if body["error"] == nil {
		t.Fatalf("unknown tool was not refused: %+v", body)
	}
}

func TestServerAcceptsANotification(t *testing.T) {
	t.Parallel()
	server := testMCP(t)
	status, _ := rpc(t, server, "token",
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	if status != http.StatusAccepted {
		t.Fatalf("notification answered %d", status)
	}
	health, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health answered %d", health.StatusCode)
	}
}

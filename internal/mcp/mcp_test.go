package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testToken          = "secret"
	methodInitialize   = "initialize"
	methodToolsList    = "tools/list"
	keyTools           = "tools"
	keyInputSchema     = "inputSchema"
	testKeyType        = "type"
	testKeyText        = "text"
	testKeyObject      = "object"
	toolGet            = "resources_get"
	toolList           = "resources_list"
	toolLogs           = "pods_log"
	keyProtocolVersion = "protocolVersion"
)

// fakeMCP is a minimal Streamable HTTP server for the client tests.
func fakeMCP(t *testing.T, handler func(method string, params json.RawMessage) (any, error)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testToken {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(envelope.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		result, err := handler(envelope.Method, envelope.Params)
		if err != nil {
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				jsonRPCField: jsonRPCVersion, "id": envelope.ID,
				"error": map[string]any{"code": -32000, "message": err.Error()},
			})
			return
		}
		response.Header().Set("Mcp-Session-Id", "session-1")
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			jsonRPCField: jsonRPCVersion, "id": envelope.ID, "result": result,
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func token(context.Context) (string, error) { return testToken, nil }

func TestClientHandshakeListAndCall(t *testing.T) {
	t.Parallel()
	server := fakeMCP(t, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case methodInitialize:
			return map[string]any{keyProtocolVersion: ProtocolVersion}, nil
		case methodToolsList:
			return map[string]any{keyTools: []map[string]any{
				{keyToolName: toolGet, "description": "read", keyInputSchema: map[string]any{testKeyType: testKeyObject}},
			}}, nil
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(params, &call)
			if call.Name != toolGet || call.Arguments[keyToolName] != "checkout" {
				return nil, errors.New("unexpected call")
			}
			return map[string]any{
				"content": []map[string]string{{testKeyType: testKeyText, "text": `{"kind":"Service"}`}},
			}, nil
		default:
			return nil, errors.New("unexpected method")
		}
	})
	client := New("test", server.URL, token, nil)
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != toolGet {
		t.Fatalf("tools %+v", tools)
	}
	result, err := client.CallTool(context.Background(), toolGet,
		json.RawMessage(`{"kind":"Service","name":"checkout"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.IsError || result.Text != `{"kind":"Service"}` {
		t.Fatalf("result %+v", result)
	}
}

func TestClientReadsAnSSEResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(request.Body).Decode(&envelope)
		if len(envelope.ID) == 0 {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{keyTools: []map[string]any{{keyToolName: toolGet}}}
		if envelope.Method == methodInitialize {
			result = map[string]any{keyProtocolVersion: ProtocolVersion}
		}
		payload, _ := json.Marshal(map[string]any{jsonRPCField: jsonRPCVersion, "id": envelope.ID, "result": result})
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(response, "event: message\ndata: %s\n\n", payload)
	}))
	t.Cleanup(server.Close)
	client := New("sse", server.URL, token, nil)
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("list over SSE: %v", err)
	}
}

func TestClientRefusesAnOversizedResponse(t *testing.T) {
	t.Parallel()
	server := fakeMCP(t, func(method string, _ json.RawMessage) (any, error) {
		return map[string]any{keyTools: []map[string]string{{keyToolName: strings.Repeat("x", 5000)}}}, nil
	})
	client := New("large", server.URL, token, nil)
	client.MaxResponseBytes = 1024
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Fatal("the oversized response was accepted")
	}
}

func TestRegistryRefusesADuplicateToolName(t *testing.T) {
	t.Parallel()
	first := &fakeAdapter{tools: []Tool{{Name: "same"}}}
	second := &fakeAdapter{tools: []Tool{{Name: "same"}}}
	if _, err := NewRegistry(context.Background(), map[string]Adapter{"a": first, "b": second}); err == nil {
		t.Fatal("a duplicate tool name was admitted")
	}
}

type fakeAdapter struct {
	tools []Tool
}

func (f *fakeAdapter) Tools(context.Context) ([]Tool, error) { return f.tools, nil }
func (f *fakeAdapter) Call(context.Context, string, json.RawMessage) (Result, error) {
	return Result{}, nil
}

// kubernetesServer advertises every kind of tool, so the policy filter is
// tested against a server that does not help it.
func kubernetesServer(t *testing.T) *httptest.Server {
	return fakeMCP(t, func(method string, _ json.RawMessage) (any, error) {
		if method == methodInitialize {
			return map[string]any{keyProtocolVersion: ProtocolVersion}, nil
		}
		if method == methodToolsList {
			return map[string]any{keyTools: []map[string]any{
				{keyToolName: toolList, keyInputSchema: map[string]any{testKeyType: testKeyObject}},
				{keyToolName: toolGet, keyInputSchema: map[string]any{testKeyType: testKeyObject}},
				{keyToolName: toolLogs, keyInputSchema: map[string]any{testKeyType: testKeyObject}},
				{keyToolName: "secrets_get", keyInputSchema: map[string]any{testKeyType: testKeyObject}},
				{keyToolName: "pods_exec", keyInputSchema: map[string]any{testKeyType: testKeyObject}},
			}}, nil
		}
		return nil, errors.New("unexpected")
	})
}

func TestKubernetesPolicyAdmitsOnlyReadTools(t *testing.T) {
	t.Parallel()
	client := New("kubernetes", kubernetesServer(t).URL, token, nil)
	policy := &Kubernetes{Client: client}
	tools, err := policy.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if names["secrets_get"] || names["pods_exec"] {
		t.Fatalf("a forbidden tool was admitted: %+v", names)
	}
	if !names[toolGet] || !names[toolLogs] {
		t.Fatalf("a read tool is missing: %+v", names)
	}
	for _, name := range []string{"secrets_get", "pods_exec", "delete_pod"} {
		if _, err := policy.Call(context.Background(), name, json.RawMessage(`{}`)); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s was not denied: %v", name, err)
		}
	}
}

func TestKubernetesPolicyRefusesSecretAndUnknownKinds(t *testing.T) {
	t.Parallel()
	policy := &Kubernetes{Client: New("kubernetes", kubernetesServer(t).URL, token, nil)}
	for _, arguments := range []string{
		`{"kind":"Secret","namespace":"shop","name":token"}`,
		`{"kind":"CertificateSigningRequest","name":x"}`,
		`{"kind":"Deployment","namespace":"shop","name":"checkout","apiVersion":"v1"}`,
	} {
		_, err := policy.Call(context.Background(), toolGet, json.RawMessage(arguments))
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("%s was not denied: %v", arguments, err)
		}
	}
}

func TestKubernetesPolicyEnforcesTheNamespaceAllowlist(t *testing.T) {
	t.Parallel()
	policy := &Kubernetes{
		Client:     New("kubernetes", kubernetesServer(t).URL, token, nil),
		Namespaces: []string{"shop"},
	}
	_, err := policy.Call(context.Background(), toolList, json.RawMessage(`{"kind":"Pod"}`))
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("a cluster-wide list bypassed the allowlist: %v", err)
	}
	_, err = policy.Call(context.Background(), toolList,
		json.RawMessage(`{"kind":"Pod","namespace":"other"}`))
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("another namespace bypassed the allowlist: %v", err)
	}
}

func TestKubernetesPolicyBoundsLogs(t *testing.T) {
	t.Parallel()
	var received map[string]any
	server := fakeMCP(t, func(method string, params json.RawMessage) (any, error) {
		if method == methodToolsList {
			return map[string]any{keyTools: []map[string]any{{keyToolName: toolLogs, keyInputSchema: map[string]any{}}}}, nil
		}
		var call struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &call)
		received = call.Arguments
		return map[string]any{"content": []map[string]string{{testKeyType: testKeyText, "text": "ok"}}}, nil
	})
	policy := &Kubernetes{Client: New("kubernetes", server.URL, token, nil)}
	if _, err := policy.Call(context.Background(), toolLogs,
		json.RawMessage(`{"namespace":"shop","name":"api","tailLines":100000}`)); err != nil {
		t.Fatalf("call: %v", err)
	}
	if received["tailLines"] != float64(MaxLogTailLines) {
		t.Fatalf("tailLines is %v", received["tailLines"])
	}
}

func TestGitHubPolicyBoundsPaths(t *testing.T) {
	t.Parallel()
	policy := &GitHub{
		Client:     New("github", kubernetesServer(t).URL, token, nil),
		PathPrefix: "environments/staging",
	}
	for _, path := range []string{
		"../secrets.yaml", "/etc/passwd", `environments\staging\x`, "environments/prod/app.yaml",
	} {
		if _, err := policy.path(path); !errors.Is(err, ErrDenied) {
			t.Fatalf("%q was accepted: %v", path, err)
		}
	}
	if _, err := policy.path("environments/staging/app.yaml"); err != nil {
		t.Fatalf("an allowed path was refused: %v", err)
	}
}

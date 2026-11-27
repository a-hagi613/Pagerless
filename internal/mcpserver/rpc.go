// Package mcpserver is the Streamable HTTP JSON-RPC server shared by the
// Kubernetes and GitHub integrations. Each process exposes only the tools its
// provider registers; there is no generic dispatch to an arbitrary process or
// endpoint.
package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProtocolVersion is the MCP revision these servers speak.
const ProtocolVersion = "2025-06-18"

const (
	maxRequestBytes = 1 << 20
	maxToolBytes    = 512 << 10
	// jsonRPCVersion is the only JSON-RPC version these servers speak.
	jsonRPCVersion = "2.0"
)

// Tool is one provider tool.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Call runs the tool and returns its text result.
	Call func(ctx context.Context, arguments json.RawMessage) (string, error)
}

// Server is one provider's MCP surface.
type Server struct {
	// Name is reported in the handshake.
	Name string
	// Tools are the provider's tools.
	Tools []Tool
	// Token returns the installer-provided bearer credential.
	Token func() (string, error)
}

// Handler serves POST /mcp and GET /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = response.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /mcp", s.handle)
	return mux
}

// handle authenticates and dispatches one JSON-RPC request.
func (s *Server) handle(response http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = response.Write([]byte(`{"error":{"code":"unauthorized","message":"unauthorized"}}`))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		writeRPCError(response, http.StatusBadRequest, nil, -32600, "the request body exceeds the bound")
		return
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.JSONRPC != jsonRPCVersion {
		writeRPCError(response, http.StatusBadRequest, nil, -32600, "the request is not JSON-RPC 2.0")
		return
	}
	if len(envelope.ID) == 0 {
		response.WriteHeader(http.StatusAccepted)
		return
	}
	switch envelope.Method {
	case "initialize":
		writeRPCResult(response, envelope.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": s.Name, "version": "1.0"},
		})
	case "ping":
		writeRPCResult(response, envelope.ID, map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, tool := range s.Tools {
			tools = append(tools, map[string]any{
				"name": tool.Name, "description": tool.Description, "inputSchema": tool.InputSchema,
			})
		}
		writeRPCResult(response, envelope.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.call(response, envelope.ID, envelope.Params)
	default:
		writeRPCError(response, http.StatusOK, envelope.ID, -32601, "the method is not supported")
	}
}

func (s *Server) call(response http.ResponseWriter, id json.RawMessage, params json.RawMessage) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		writeRPCError(response, http.StatusOK, id, -32602, "the tool call parameters are not valid")
		return
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}
	for _, tool := range s.Tools {
		if tool.Name != call.Name {
			continue
		}
		text, err := tool.Call(context.Background(), call.Arguments)
		if err != nil {
			writeToolResult(response, id, "the tool call failed: "+err.Error(), true)
			return
		}
		if len(text) > maxToolBytes {
			writeToolResult(response, id, "the result exceeds the integration's size bound", true)
			return
		}
		writeToolResult(response, id, text, false)
		return
	}
	writeRPCError(response, http.StatusOK, id, -32602, "the tool is not admitted")
}

func (s *Server) authorized(request *http.Request) bool {
	if s.Token == nil {
		return false
	}
	expected, err := s.Token()
	if err != nil || expected == "" {
		return false
	}
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(strings.TrimSpace(token))) == 1
}

func writeToolResult(response http.ResponseWriter, id json.RawMessage, text string, isError bool) {
	writeRPCResult(response, id, map[string]any{
		"isError": isError,
		"content": []map[string]string{{"type": "text", "text": text}},
	})
}

func writeRPCResult(response http.ResponseWriter, id json.RawMessage, result any) {
	writeRPC(response, http.StatusOK, map[string]any{
		"jsonrpc": jsonRPCVersion, "id": id, "result": result,
	})
}

func writeRPCError(response http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeRPC(response, status, map[string]any{
		"jsonrpc": jsonRPCVersion, "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func writeRPC(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

// Ensure the errors package is used when a provider wants a typed refusal.
var ErrRefused = errors.New("refused")

// Refuse wraps a message as a tool-level refusal.
func Refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

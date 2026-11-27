package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/internal/config"
	"github.com/a-hagi613/pagerless/internal/investigations"
	"github.com/a-hagi613/pagerless/internal/mcpserver"
)

// newTriggerServer exposes the automation surface: one MCP tool and the same
// payload as a notification. The trigger token is separate from the operator
// token when the installation Secret carries one; an existing installation
// without it falls back to the operator token so an upgrade cannot lock the
// endpoint out.
func newTriggerServer(
	cfg config.Config,
	service *investigations.Service,
	operatorToken func() (string, error),
) *mcpserver.Server {
	triggerToken := operatorToken
	if strings.TrimSpace(cfg.TriggerTokenFile) != "" {
		candidate := config.ReadToken(cfg.TriggerTokenFile)
		if _, err := candidate(); err == nil {
			triggerToken = candidate
		}
	}
	tool := mcpserver.Tool{
		Name:        "pagerless_investigate",
		Description: "Start a Pagerless investigation. Returns the page and turn IDs.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"question":{"type":"string","description":"The incident question or alert summary"},
				"namespace":{"type":"string"},
				"target":{
					"type":"object",
					"properties":{
						"kind":{"type":"string"},
						"name":{"type":"string"},
						"namespace":{"type":"string"}
					},
					"required":["kind","name"],
					"additionalProperties":false
				},
				"idempotencyKey":{"type":"string","description":"Stable alert fingerprint for retries"}
			},
			"required":["question"],
			"additionalProperties":false
		}`),
		Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
			return investigate(ctx, service, raw)
		},
	}
	notifications := map[string]func(context.Context, json.RawMessage) error{
		"notifications/pagerless/investigate": func(ctx context.Context, params json.RawMessage) error {
			_, err := investigate(ctx, service, params)
			if err != nil {
				slog.Warn("the MCP investigation notification failed", "error", err)
			}
			return err
		},
	}
	return &mcpserver.Server{
		Name:          "pagerless",
		Token:         triggerToken,
		Tools:         []mcpserver.Tool{tool},
		Notifications: notifications,
	}
}

// investigate admits one page from an MCP client or notification. The caller
// never supplies a credential; the MCP bearer authenticates the integration.
func investigate(ctx context.Context, service *investigations.Service, raw json.RawMessage) (string, error) {
	var arguments struct {
		Question       string `json:"question"`
		Namespace      string `json:"namespace"`
		IdempotencyKey string `json:"idempotencyKey"`
		Target         *struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"target"`
	}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return "", fmt.Errorf("the arguments are not a JSON object")
	}
	if strings.TrimSpace(arguments.Question) == "" {
		return "", fmt.Errorf("question is required")
	}
	key := strings.TrimSpace(arguments.IdempotencyKey)
	if key == "" {
		key = randomKey()
	}
	request := investigations.Request{Message: arguments.Question, Namespace: arguments.Namespace}
	if arguments.Target != nil {
		request.Target = &investigations.Target{
			Kind: arguments.Target.Kind, Name: arguments.Target.Name, Namespace: arguments.Target.Namespace,
		}
	}
	created, err := service.CreatePage(ctx, key, request)
	if err != nil {
		return "", err
	}
	response, err := json.Marshal(map[string]string{
		"pageId": created.PageID,
		"turnId": created.TurnID,
		"url":    "/pages/" + created.PageID,
	})
	if err != nil {
		return "", fmt.Errorf("encode the created page")
	}
	return string(response), nil
}

func randomKey() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "mcp-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "mcp-" + hex.EncodeToString(buffer)
}

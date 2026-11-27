package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInvestigateRejectsMalformedArguments(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{}`,
		`{"question":"  "}`,
		`[]`,
	}
	for _, raw := range cases {
		if _, err := investigate(context.Background(), nil, json.RawMessage(raw)); err == nil {
			t.Fatalf("the arguments %s were accepted", raw)
		}
	}
}

func TestRandomKeyIsAStableShapedIdentifier(t *testing.T) {
	t.Parallel()
	key := randomKey()
	if !strings.HasPrefix(key, "mcp-") || len(key) != len("mcp-")+32 {
		t.Fatalf("random key %q", key)
	}
	if key == randomKey() {
		t.Fatal("two random keys are equal")
	}
}

package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONKeepsScalarsAndRedactsNestedStrings(t *testing.T) {
	t.Parallel()
	document := []byte(`{"generation":1,"ready":true,"ratio":0.5,"missing":null,` +
		`"nested":{"password":"hunter2"},"items":["token=abcdef123456"]}`)
	redacted, count := Bytes(document)
	if count != 2 {
		t.Fatalf("redacted %d matches, want 2", count)
	}
	if strings.Contains(string(redacted), "hunter2") || strings.Contains(string(redacted), "abcdef123456") {
		t.Fatalf("a secret survived: %s", redacted)
	}
	var decoded map[string]any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("the redacted document is not JSON: %v", err)
	}
	if decoded["generation"] != float64(1) || decoded["ready"] != true || decoded["ratio"] != 0.5 {
		t.Fatalf("a scalar was replaced: %+v", decoded)
	}
}

func TestTextLeavesOrdinaryTextAlone(t *testing.T) {
	t.Parallel()
	text, count := Text("the checkout deployment has no memory limit")
	if count != 0 || text != "the checkout deployment has no memory limit" {
		t.Fatalf("text=%q count=%d", text, count)
	}
}

package tools

import (
	"encoding/json"
	"testing"
)

// decodeData decodes a result's data into a generic map.
func decodeData(t *testing.T, result Result) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestPodLogsReadsThePreviousContainerFirst covers the crash-loop evidence rule.
func TestPodLogsReadsThePreviousContainerFirst(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	result, refused := call(t, broker, page, ToolPodsLogs, "logs", map[string]any{})
	if refused != nil {
		t.Fatalf("pods_logs was refused: %+v", refused)
	}
	data := decodeData(t, result)
	pods, ok := data["pods"].([]any)
	if !ok || len(pods) != 1 {
		t.Fatalf("data = %+v, want the Target's one pod", data)
	}
	pod := pods[0].(map[string]any)
	if pod["previous"] != true {
		t.Errorf("previous = %v, want the previous container read first", pod["previous"])
	}
	lines, _ := pod["lines"].([]any)
	if len(lines) != 2 || lines[1] != testKilledLine {
		t.Errorf("lines = %v, want the previous instance's log", lines)
	}
}

// TestEventsReadBothEventAPIs covers the dual-API read.
func TestEventsReadBothEventAPIs(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	result, refused := call(t, broker, page, ToolEventsList, "events", map[string]any{"sinceSeconds": 86400})
	if refused != nil {
		t.Fatalf("events_list was refused: %+v", refused)
	}
	data := decodeData(t, result)
	events, _ := data["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("data = %+v, want the Target's Event", data)
	}
	first := events[0].(map[string]any)
	if first["source"] != "core/v1" || first["reason"] != testReason {
		t.Errorf("event = %+v, want the core/v1 BackOff Event", first)
	}
}

// TestOwnerChainWalksToTheDeployment covers the owner chain.
func TestOwnerChainWalksToTheDeployment(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, _ := brokerFixture(t)
	result, refused := call(t, broker, page, ToolOwnerChain, "own", map[string]any{
		fieldKind: kindPod, fieldName: testPodName,
	})
	if refused != nil {
		t.Fatalf("owner_chain was refused: %+v", refused)
	}
	data := decodeData(t, result)
	chain, _ := data["chain"].([]any)
	if len(chain) != 3 {
		t.Fatalf("chain = %+v, want Pod, ReplicaSet, Deployment", chain)
	}
	last := chain[2].(map[string]any)
	if last["kind"] != kindDeployment || last["name"] != "checkout" {
		t.Errorf("chain end = %+v, want the Deployment", last)
	}
}

// TestRepositoryReadsUseThePagesRepository covers the GitHub read scope.
func TestRepositoryReadsUseThePagesRepository(t *testing.T) {
	t.Parallel()
	page := pageFixture()
	broker, _, _, github := brokerFixture(t)
	if _, refused := call(t, broker, page, ToolRepoFile, "file", map[string]any{
		"path": testManifestPath,
	}); refused != nil {
		t.Fatalf("repo_file was refused: %+v", refused)
	}
	if github.repository.Owner != "acme" || github.repository.Name != "gitops" || github.ref != testRef {
		t.Errorf("repository = %+v, want acme/gitops at main", github.repository)
	}
}

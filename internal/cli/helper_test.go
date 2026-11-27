package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Fixtures shared by the command tests.
const (
	testToken       = "test-token"
	testTool        = "resources_get"
	testLogsTool    = "logs_get"
	testKind        = "Deployment"
	testCheckout    = "checkout"
	testNamespace   = "shop"
	testQuestion    = "Why is checkout slow?"
	testCapacity    = "capacity"
	testUnavailable = "unavailable"

	fieldCallID     = "callId"
	fieldTool       = "tool"
	fieldStatus     = "status"
	fieldContent    = "content"
	fieldArguments  = "arguments"
	fieldDurationMS = "durationMs"
	fieldTruncated  = "truncated"
)

// result is one command's streams and error.
type result struct {
	stdout string
	stderr string
	err    error
}

// runCLI executes a fresh root command with captured streams.
func runCLI(t *testing.T, ctx context.Context, args ...string) result {
	t.Helper()
	return runCLIInput(t, ctx, "", args...)
}

// runCLIInput executes a fresh root command with standard input set.
func runCLIInput(t *testing.T, ctx context.Context, input string, args ...string) result {
	t.Helper()
	root := NewRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if input != "" {
		root.SetIn(bytes.NewBufferString(input))
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return result{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// prepare sets the environment that keeps a test off the network and out of the
// real user's state directory.
func prepare(t *testing.T) {
	t.Helper()
	t.Setenv("PAGERLESS_TOKEN", testToken)
	t.Setenv("PAGERLESS_TOKEN_FILE", "")
	t.Setenv("PAGERLESS_ADDRESS", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

// eventJSON renders one event with a fixed page, turn, and timestamp.
func eventJSON(t *testing.T, seq int64, eventType string, data any) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Seq    int64  `json:"seq"`
		PageID string `json:"pageId"`
		TurnID string `json:"turnId"`
		Type   string `json:"type"`
		Time   string `json:"time"`
		Data   any    `json:"data"`
	}{Seq: seq, PageID: "p1", TurnID: "t1", Type: eventType,
		Time: "2026-01-01T00:00:00Z", Data: data})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(raw)
}

// sseEvent is one event with the sequence used for its SSE id.
type sseEvent struct {
	seq     int64
	payload string
}

// writeSSE writes a complete event stream and flushes it.
func writeSSE(t *testing.T, writer http.ResponseWriter, events []sseEvent) {
	t.Helper()
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, ok := writer.(http.Flusher)
	if !ok {
		t.Fatalf("the test server cannot flush")
	}
	for _, item := range events {
		if _, err := fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", item.seq, item.payload); err != nil {
			return
		}
	}
	flusher.Flush()
}

// newServer starts a test server and closes it with the test.
func newServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// decode unmarshals a JSON document and fails the test on error.
func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode JSON %q: %v", raw, err)
	}
	return value
}

// requireCode asserts a documented exit code.
func requireCode(t *testing.T, err error, want int) {
	t.Helper()
	if got := ExitCode(err); got != want {
		t.Fatalf("exit code = %d, want %d (error: %v)", got, want, err)
	}
}

// containsAll asserts every wanted substring appears.
func containsAll(t *testing.T, text string, wanted ...string) {
	t.Helper()
	for _, want := range wanted {
		if !bytes.Contains([]byte(text), []byte(want)) {
			t.Fatalf("%q does not contain %q", text, want)
		}
	}
}

// excludesAll asserts none of the unwanted substrings appears.
func excludesAll(t *testing.T, text string, unwanted ...string) {
	t.Helper()
	for _, item := range unwanted {
		if bytes.Contains([]byte(text), []byte(item)) {
			t.Fatalf("%q unexpectedly contains %q", text, item)
		}
	}
}

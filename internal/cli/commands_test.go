package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAskStreamsProgressSeparatelyAndWritesJSON(t *testing.T) {
	prepare(t)
	var created createRequest
	var idempotencyKey string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pages", func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		idempotencyKey = request.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"pageId":"p1","turnId":"t1"}`))
	})
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		if got := request.URL.Query().Get("after"); got != "0" {
			t.Errorf("after = %q", got)
		}
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventToolStarted, map[string]any{
				fieldCallID: "c1", fieldTool: testTool, fieldArguments: map[string]string{"kind": "Service"},
			})},
			{2, eventJSON(t, 2, eventToolFinished, map[string]any{
				fieldCallID: "c1", fieldTool: testTool, fieldStatus: "ok", fieldDurationMS: 120, fieldTruncated: true,
			})},
			{3, eventJSON(t, 3, eventAnswer, map[string]any{fieldContent: testAnswer})},
			{4, eventJSON(t, 4, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	mux.HandleFunc("GET /api/v1/pages/p1", func(writer http.ResponseWriter, _ *http.Request) {
		detail := pageDetail{
			ID: "p1", Title: testQuestion, Status: statusCompleted,
			UpdatedAt: time.Date(2026, 1, 1, 0, 0, 4, 0, time.UTC), LastEventID: 4,
			Messages: []message{
				{ID: "m1", TurnID: "t1", Role: "user", Content: testQuestion},
				{ID: "m2", TurnID: "t1", Role: roleAssistant, Content: testAnswer,
					Citations: []citation{{CallID: "c1", Pointer: "/subsets"}}},
			},
		}
		_ = json.NewEncoder(writer).Encode(detail)
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "ask",
		"--target", "Deployment/checkout", "-n", testNamespace, "Why", "is", testCheckout, "slow?", "--json")
	if res.err != nil {
		t.Fatalf("ask: %v (stderr %s)", res.err, res.stderr)
	}
	detail := decode[pageDetail](t, res.stdout)
	if len(detail.Messages) != 2 || detail.LastEventID != 4 {
		t.Fatalf("detail = %+v", detail)
	}
	if created.Message != testQuestion || created.Namespace != testNamespace {
		t.Fatalf("create body = %+v", created)
	}
	if created.Target == nil || created.Target.Kind != testKind || created.Target.Name != testCheckout {
		t.Fatalf("target = %+v", created.Target)
	}
	if len(idempotencyKey) != 32 {
		t.Fatalf("Idempotency-Key = %q", idempotencyKey)
	}
	containsAll(t, res.stderr, "page p1 turn t1", "-> resources_get")
	excludesAll(t, res.stdout, testTool, "->", testToken)
}

func TestHumanAskKeepsProgressOffStandardOutputAndEscapesControlText(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pages", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"pageId":"p1","turnId":"t1"}`))
	})
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: testLogsTool})},
			{2, eventJSON(t, 2, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	mux.HandleFunc("GET /api/v1/pages/p1", func(writer http.ResponseWriter, _ *http.Request) {
		detail := pageDetail{ID: "p1", Status: statusCompleted, LastEventID: 2, Messages: []message{
			{ID: "m1", Role: roleAssistant, Content: "\x1b[31mred\x1b[0m answer"},
		}}
		_ = json.NewEncoder(writer).Encode(detail)
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "Why?")
	if res.err != nil {
		t.Fatalf("ask: %v", res.err)
	}
	containsAll(t, res.stdout, `\x1b[31mred`, eventAnswer)
	excludesAll(t, res.stdout, "\x1b", "-> logs_get")
	containsAll(t, res.stderr, "-> logs_get")
}

func TestReplyAddsATurnWithAnIdempotencyKey(t *testing.T) {
	prepare(t)
	var key, message string
	var inherited createRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pages/p1/turns", func(writer http.ResponseWriter, request *http.Request) {
		key = request.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(request.Body).Decode(&inherited)
		message = inherited.Message
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"pageId":"p1","turnId":"t2"}`))
	})
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{3, eventJSON(t, 3, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	mux.HandleFunc("GET /api/v1/pages/p1", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(pageDetail{ID: "p1", Status: statusCompleted, LastEventID: 3})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "reply", "p1", "What", "changed?", "--json")
	if res.err != nil {
		t.Fatalf("reply: %v", res.err)
	}
	if message != "What changed?" || len(key) != 32 {
		t.Fatalf("message = %q, key = %q", message, key)
	}
	if inherited.Target != nil || inherited.Namespace != "" {
		t.Fatalf("reply should inherit the page's hints, not resend them: %+v", inherited)
	}
}

func TestAskReportsAFailedTurnAndKeepsThePage(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pages", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"pageId":"p1","turnId":"t1"}`))
	})
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventTurnFinished, map[string]any{fieldStatus: statusFailed})},
		})
	})
	mux.HandleFunc("GET /api/v1/pages/p1", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(pageDetail{ID: "p1", Status: statusFailed, LastEventID: 1})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "ask", "why?")
	requireCode(t, res.err, ExitFailure)
	containsAll(t, res.stdout, "ID:", "p1", "failed")
	containsAll(t, ErrorText(res.err), "failed")
}

func TestDetachExplainsHowToResumeAndStop(t *testing.T) {
	root := NewRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	instance := &app{opts: &options{}, cmd: root}

	if err := instance.reportDetach("p1", "t1", streamOutcome{LastSeq: 9}); err != nil {
		t.Fatalf("reportDetach: %v", err)
	}
	containsAll(t, stderr.String(), "Detached", "event 9", "kubectl page follow p1", "kubectl page stop p1")

	instance.opts.json = true
	stdout.Reset()
	if err := instance.reportDetach("p1", "t1", streamOutcome{LastSeq: 9}); err != nil {
		t.Fatalf("reportDetach JSON: %v", err)
	}
	body := decode[struct {
		PageID      string `json:"pageId"`
		TurnID      string `json:"turnId"`
		Detached    bool   `json:"detached"`
		LastEventID int64  `json:"lastEventId"`
	}](t, stdout.String())
	if body.PageID != "p1" || body.TurnID != "t1" || !body.Detached || body.LastEventID != 9 {
		t.Fatalf("detach JSON = %+v", body)
	}
}

func TestFollowHumanPrintsTheTraceToStandardOutput(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: testTool})},
			{2, eventJSON(t, 2, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1", "--from-start")
	if res.err != nil {
		t.Fatalf("follow: %v", res.err)
	}
	containsAll(t, res.stdout, "[1]", eventToolStarted, testTool, "[2]", eventTurnFinished, statusCompleted)
	excludesAll(t, res.stdout, "\x1b")
}

func TestShowHumanRendersMessagesAndCitations(t *testing.T) {
	prepare(t)
	server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(pageDetail{
			ID: "p1", Title: testQuestion, Status: statusCompleted, LastEventID: 4,
			Messages: []message{
				{ID: "m1", Role: "user", Content: testQuestion},
				{ID: "m2", Role: roleAssistant, Content: "No endpoints.",
					Citations: []citation{{CallID: "c1", Pointer: "/subsets"}}},
			},
		})
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "show", "p1")
	if res.err != nil {
		t.Fatalf("show: %v", res.err)
	}
	containsAll(t, res.stdout, "p1", testQuestion, "No endpoints.", "citation: c1 /subsets", "Last event")
}

func TestListRendersOrderAndJSON(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"pages": []pageSummary{
			{ID: "p2", Title: "Second", Status: "running", UpdatedAt: time.Now()},
			{ID: "p1", Title: "First", Status: statusCompleted, UpdatedAt: time.Now().Add(-time.Hour)},
		}})
	})
	server := newServer(t, mux)
	human := runCLI(t, context.Background(), "--address", server.URL, "list")
	if human.err != nil {
		t.Fatalf("list: %v", human.err)
	}
	containsAll(t, human.stdout, "ID", "STATUS", "UPDATED", "TITLE", "p2", "p1")
	if strings.Index(human.stdout, "p2") > strings.Index(human.stdout, "p1") {
		t.Fatalf("list reordered the server's newest-first rows:\n%s", human.stdout)
	}
	machine := runCLI(t, context.Background(), "--address", server.URL, "list", "--json")
	if machine.err != nil {
		t.Fatalf("list --json: %v", machine.err)
	}
	var list struct {
		Pages []pageSummary `json:"pages"`
	}
	if err := json.Unmarshal([]byte(machine.stdout), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Pages) != 2 || list.Pages[0].ID != "p2" {
		t.Fatalf("pages = %+v", list.Pages)
	}
	limited := runCLI(t, context.Background(), "--address", server.URL, "list", "--limit", "1")
	containsAll(t, limited.stdout, "p2")
	excludesAll(t, limited.stdout, "p1")
}

func TestShowEscapesOpaqueIdentifiersInTheRoute(t *testing.T) {
	prepare(t)
	pageID := "p 1/x"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/", func(writer http.ResponseWriter, request *http.Request) {
		want := "/api/v1/pages/" + "p%201%2Fx"
		if got := request.URL.EscapedPath(); got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		_ = json.NewEncoder(writer).Encode(pageDetail{ID: pageID, Status: statusCompleted})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "show", pageID, "--json")
	if res.err != nil {
		t.Fatalf("show: %v", res.err)
	}
}

func TestAPIErrorStatusesMapToDocumentedExitCodes(t *testing.T) {
	prepare(t)
	cases := []struct {
		name       string
		status     int
		code       string
		wantExit   int
		wantInText string
	}{
		{"unauthorized", http.StatusUnauthorized, "unauthorized", ExitAuth, "PAGERLESS_TOKEN"},
		{"forbidden", http.StatusForbidden, "forbidden", ExitAuth, "not allowed"},
		{"missing", http.StatusNotFound, "not_found", ExitNotFound, "no such page"},
		{"conflict", http.StatusConflict, "page_busy", ExitConflict, "follow or stop"},
		{"invalid", http.StatusBadRequest, "invalid_request", ExitUsage, "bad target"},
		{testCapacity, http.StatusTooManyRequests, testCapacity, ExitFailure, testCapacity},
		{"server", http.StatusBadGateway, testUnavailable, ExitFailure, testUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(testCase.status)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"error": map[string]string{"code": testCase.code, "message": messageFor(testCase.status)},
				})
			}))
			res := runCLI(t, context.Background(), "--address", server.URL, "list")
			requireCode(t, res.err, testCase.wantExit)
			containsAll(t, ErrorText(res.err), testCase.wantInText)
			excludesAll(t, ErrorText(res.err)+res.stdout+res.stderr, testToken)
		})
	}
}

// messageFor gives each status a message the test can recognise.
func messageFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "the operator token is invalid"
	case http.StatusForbidden:
		return "this token is not allowed to read history"
	case http.StatusNotFound:
		return "no such page"
	case http.StatusConflict:
		return "an active turn already exists"
	case http.StatusBadRequest:
		return "bad target"
	case http.StatusTooManyRequests:
		return "capacity exhausted"
	default:
		return testUnavailable
	}
}

func TestRequestsAreBoundedByTheTimeout(t *testing.T) {
	prepare(t)
	server := newServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	start := time.Now()
	res := runCLI(t, context.Background(), "--address", server.URL, "list", "--timeout", "40ms")
	requireCode(t, res.err, ExitFailure)
	containsAll(t, ErrorText(res.err), "timed out")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the bound took %s", elapsed)
	}
}

func TestEvidenceDiffAndStop(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/evidence/c%201", func(writer http.ResponseWriter, request *http.Request) {
		if got := request.URL.EscapedPath(); got != "/api/v1/pages/p1/evidence/c%201" {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewEncoder(writer).Encode(evidence{
			CallID: "c 1", Tool: testTool, Status: "ok", DurationMS: 12,
			Arguments: json.RawMessage(`{"kind":"Service"}`),
			Result:    json.RawMessage(`{"items":[]}`),
		})
	})
	mux.HandleFunc("GET /api/v1/pages/p1/diff", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(diffResult{
			PRURL: "https://github.com/acme/repo/pull/7",
			Files: []diffFile{{Path: "apps/shop/deployment.yaml", Diff: "--- a\n+++ b\n"}},
		})
	})
	mux.HandleFunc("POST /api/v1/pages/p1/stop", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"status":"stopped"}`))
	})
	server := newServer(t, mux)

	evidenceResult := runCLI(t, context.Background(), "--address", server.URL, "evidence", "p1", "c 1")
	if evidenceResult.err != nil {
		t.Fatalf("evidence: %v", evidenceResult.err)
	}
	containsAll(t, evidenceResult.stdout, testTool, "12ms", `"kind": "Service"`, `"items": []`)

	diffResult := runCLI(t, context.Background(), "--address", server.URL, "diff", "p1")
	if diffResult.err != nil {
		t.Fatalf("diff: %v", diffResult.err)
	}
	containsAll(t, diffResult.stdout, "pull/7", "apps/shop/deployment.yaml", "+++ b")

	for range 2 {
		stopResult := runCLI(t, context.Background(), "--address", server.URL, "stop", "p1")
		if stopResult.err != nil {
			t.Fatalf("stop: %v", stopResult.err)
		}
		containsAll(t, stopResult.stdout, "Stopped")
	}
}

func TestDiffWithoutAProposalIsExplicit(t *testing.T) {
	prepare(t)
	server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"files":[]}`))
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "diff", "p1")
	if res.err != nil {
		t.Fatalf("diff: %v", res.err)
	}
	containsAll(t, res.stdout, "No proposed change.")
}

func TestDashboardPrintsTheAddressWithoutAToken(t *testing.T) {
	prepare(t)
	t.Setenv("PAGERLESS_TOKEN", "")
	server := newServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("dashboard must not call the API before the browser does")
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "dashboard", "--json")
	if res.err != nil {
		t.Fatalf("dashboard: %v", res.err)
	}
	body := decode[struct {
		URL    string `json:"url"`
		Tunnel bool   `json:"tunnel"`
	}](t, res.stdout)
	if body.URL != server.URL+"/" || body.Tunnel {
		t.Fatalf("dashboard = %+v", body)
	}
}

func TestVersionKeepsTheReleaseContract(t *testing.T) {
	prepare(t)
	res := runCLI(t, context.Background(), "version")
	if res.err != nil {
		t.Fatalf("version: %v", res.err)
	}
	lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
	if lines[0] != "kubectl-page dev" || lines[1] != "commit none" || lines[2] != "built unknown" {
		t.Fatalf("version output = %q", res.stdout)
	}
	machine := runCLI(t, context.Background(), "version", "--json")
	info := decode[versionInfo](t, machine.stdout)
	if info.API != "/api/v1" || info.Version != Version {
		t.Fatalf("version info = %+v", info)
	}
}

func TestRetiredCommandsAreGone(t *testing.T) {
	root := NewRootCommand()
	present := map[string]bool{}
	for _, command := range root.Commands() {
		present[command.Name()] = true
	}
	for _, retired := range []string{"approve", "reject", "dismiss", "config", "login", "new", "get"} {
		if present[retired] {
			t.Errorf("retired command %q is still registered", retired)
		}
	}
	for _, required := range []string{
		"ask", "list", "show", "reply", "follow", "activity", "evidence", "diff", "stop", "dashboard", "version",
	} {
		if !present[required] {
			t.Errorf("command %q is missing", required)
		}
	}
}

func TestTokenFileAndStandardInput(t *testing.T) {
	prepare(t)
	t.Setenv("PAGERLESS_TOKEN", "")
	var authorizations []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages", func(writer http.ResponseWriter, request *http.Request) {
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		_, _ = writer.Write([]byte(`{"pages":[]}`))
	})
	server := newServer(t, mux)

	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("file-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, context.Background(), "--address", server.URL, "--token-file", path, "list")
	if res.err != nil {
		t.Fatalf("token file: %v", res.err)
	}
	containsAll(t, res.stderr, "chmod 600")
	excludesAll(t, res.stdout+res.stderr, "file-token")

	stdin := runCLIInput(t, context.Background(), "stdin-token\n",
		"--address", server.URL, "--token-file", "-", "list")
	if stdin.err != nil {
		t.Fatalf("standard input token: %v", stdin.err)
	}
	want := []string{"Bearer file-token", "Bearer stdin-token"}
	if fmt.Sprint(authorizations) != fmt.Sprint(want) {
		t.Fatalf("Authorization headers = %v, want %v", authorizations, want)
	}
}

func TestMissingTokenIsAnAuthFailure(t *testing.T) {
	prepare(t)
	t.Setenv("PAGERLESS_TOKEN", "")
	server := newServer(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("the command must not call the API without a token")
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "list")
	requireCode(t, res.err, ExitAuth)
	containsAll(t, ErrorText(res.err), "PAGERLESS_TOKEN", "--token-file")
}

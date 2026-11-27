package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFollowReconnectsFromTheLastSequenceWithoutDuplicates(t *testing.T) {
	prepare(t)
	var mutex sync.Mutex
	var afters, lastEventIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, request *http.Request) {
		after := request.URL.Query().Get("after")
		mutex.Lock()
		afters = append(afters, after)
		lastEventIDs = append(lastEventIDs, request.Header.Get("Last-Event-ID"))
		mutex.Unlock()
		switch after {
		case "0":
			writeSSE(t, writer, []sseEvent{
				{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: testLogsTool})},
				{2, eventJSON(t, 2, eventToolFinished, map[string]any{
					fieldCallID: "c1", fieldTool: testLogsTool, fieldStatus: "ok",
				})},
			})
		case "2":
			writeSSE(t, writer, []sseEvent{
				{3, eventJSON(t, 3, eventAnswer, map[string]any{fieldContent: "done"})},
				{4, eventJSON(t, 4, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
			})
		default:
			t.Errorf("unexpected after %q", after)
		}
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1", "--from-start", "--json")
	if res.err != nil {
		t.Fatalf("follow: %v (stderr %s)", res.err, res.stderr)
	}
	sequences := make([]int64, 0, 4)
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		sequences = append(sequences, decode[event](t, line).Seq)
	}
	if want := []int64{1, 2, 3, 4}; len(sequences) != len(want) ||
		sequences[0] != want[0] || sequences[3] != want[3] {
		t.Fatalf("sequence = %v, want %v", sequences, want)
	}
	if strings.Join(afters, ",") != "0,2" {
		t.Fatalf("after values = %v", afters)
	}
	if strings.Join(lastEventIDs, ",") != ",2" {
		t.Fatalf("Last-Event-ID values = %v", lastEventIDs)
	}
}

func TestStreamDropsSequencesItAlreadyDelivered(t *testing.T) {
	prepare(t)
	var calls int
	var mutex sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		calls++
		call := calls
		mutex.Unlock()
		if call == 1 {
			writeSSE(t, writer, []sseEvent{
				{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: "a"})},
				{2, eventJSON(t, 2, eventToolFinished, map[string]any{fieldCallID: "c1", fieldTool: "a", fieldStatus: "ok"})},
			})
			return
		}
		// A replaying server resends the acknowledged events.
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: "a"})},
			{2, eventJSON(t, 2, eventToolFinished, map[string]any{fieldCallID: "c1", fieldTool: "a", fieldStatus: "ok"})},
			{3, eventJSON(t, 3, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1", "--from-start", "--json")
	if res.err != nil {
		t.Fatalf("follow: %v", res.err)
	}
	sequences := make([]int64, 0, 3)
	for line := range strings.SplitSeq(strings.TrimSpace(res.stdout), "\n") {
		sequences = append(sequences, decode[event](t, line).Seq)
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[1] != 2 || sequences[2] != 3 {
		t.Fatalf("sequence = %v", sequences)
	}
}

func TestCtrlCDetachesWithoutStoppingTheTurn(t *testing.T) {
	var stops int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, request *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: "a"})},
			{2, eventJSON(t, 2, eventToolFinished, map[string]any{fieldCallID: "c1", fieldTool: "a", fieldStatus: "ok"})},
		})
		<-request.Context().Done()
	})
	mux.HandleFunc("POST /api/v1/pages/p1/stop", func(writer http.ResponseWriter, _ *http.Request) {
		stops++
		_, _ = writer.Write([]byte(`{"status":"stopped"}`))
	})
	server := newServer(t, mux)
	api, err := newClient(server.URL, testToken, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delivered []int64
	outcome, err := api.follow(ctx, "p1", 0, func(current event) {
		delivered = append(delivered, current.Seq)
		if current.Seq == 2 {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if !outcome.Detached || outcome.LastSeq != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if len(delivered) != 2 || stops != 0 {
		t.Fatalf("delivered = %v, stops = %d", delivered, stops)
	}
}

func TestFollowTimeoutIsAnErrorWithAResumePoint(t *testing.T) {
	prepare(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, request *http.Request) {
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventTurnStarted, map[string]any{})},
		})
		<-request.Context().Done()
	})
	server := newServer(t, mux)
	start := time.Now()
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1",
		"--from-start", "--follow-timeout", "80ms")
	requireCode(t, res.err, ExitFailure)
	containsAll(t, ErrorText(res.err), "stopped waiting", "--after 1")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestCursorIsHashedAndDrivesResume(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const pageID = "../../etc/passwd"
	if err := saveCursor(pageID, 5); err != nil {
		t.Fatalf("save cursor: %v", err)
	}
	path, ok := cursorFile(pageID)
	if !ok {
		t.Fatal("no cursor path")
	}
	wantPrefix := filepath.Join(state, "pagerless", "cursors") + string(os.PathSeparator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Fatalf("cursor path %q escaped %q", path, wantPrefix)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cursor file: %v", err)
	}
	if seq, ok := loadCursor(pageID); !ok || seq != 5 {
		t.Fatalf("load cursor = %d, %v", seq, ok)
	}
	if got := startingSequence(pageID, -1, false); got != 5 {
		t.Fatalf("resume = %d", got)
	}
	if got := startingSequence(pageID, 2, false); got != 2 {
		t.Fatalf("explicit after = %d", got)
	}
	if got := startingSequence(pageID, -1, true); got != 0 {
		t.Fatalf("from start = %d", got)
	}
}

func TestFollowResumesFromTheStoredCursor(t *testing.T) {
	prepare(t)
	if err := saveCursor("p1", 7); err != nil {
		t.Fatal(err)
	}
	var after string
	server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		after = request.URL.Query().Get("after")
		writeSSE(t, writer, []sseEvent{
			{8, eventJSON(t, 8, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1")
	if res.err != nil {
		t.Fatalf("follow: %v", res.err)
	}
	if after != "7" {
		t.Fatalf("after = %q, want the stored cursor", after)
	}
	if seq, ok := loadCursor("p1"); !ok || seq != 8 {
		t.Fatalf("cursor = %d, %v", seq, ok)
	}
}

func TestAddressValidation(t *testing.T) {
	cases := []string{
		"http://user:secret@example.test",
		"https://example.test/?token=secret",
		"https://example.test/#fragment",
		"ftp://example.test",
		"example.test",
	}
	for _, address := range cases {
		t.Run(address, func(t *testing.T) {
			_, err := newClient(address, "token", time.Second)
			requireCode(t, err, ExitUsage)
		})
	}
}

func TestDuplicateSubmissionReturnsConflictWithAFreshKey(t *testing.T) {
	var mutex sync.Mutex
	var keys []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pages", func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		first := len(keys) == 1
		mutex.Unlock()
		if first {
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"pageId":"p1","turnId":"t1"}`))
			return
		}
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":{"code":"page_busy","message":"an active turn exists"}}`))
	})
	server := newServer(t, mux)
	api, err := newClient(server.URL, "token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := api.createPage(context.Background(), createRequest{Message: "why?"})
	if err != nil || first.PageID != "p1" {
		t.Fatalf("first create = %+v, %v", first, err)
	}
	_, err = api.createPage(context.Background(), createRequest{Message: "why?"})
	requireCode(t, err, ExitConflict)
	containsAll(t, ErrorText(err), "follow or stop")
	if len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Fatalf("keys = %v", keys)
	}
}

func TestResponseSizeIsBounded(t *testing.T) {
	prepare(t)
	server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"pages":["`))
		chunk := strings.Repeat("a", 1<<20)
		for range 9 {
			_, _ = writer.Write([]byte(chunk))
		}
		_, _ = writer.Write([]byte(`"]}`))
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "list")
	requireCode(t, res.err, ExitFailure)
	containsAll(t, ErrorText(res.err), "exceeds")
}

func TestStreamAcceptsATerminalEventWithoutATrailingBlankLine(t *testing.T) {
	prepare(t)
	server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, "id: 1\ndata: %s\n", eventJSON(t, 1, eventTurnFinished,
			map[string]any{fieldStatus: statusCompleted}))
	}))
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1", "--from-start", "--json")
	if res.err != nil {
		t.Fatalf("follow: %v", res.err)
	}
	if decode[event](t, strings.TrimSpace(res.stdout)).Seq != 1 {
		t.Fatalf("event = %q", res.stdout)
	}
}

func TestStreamTreatsAReplayedTerminalEventAsFinished(t *testing.T) {
	prepare(t)
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeSSE(t, writer, []sseEvent{
				{1, eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: testTool})},
			})
			return
		}
		writeSSE(t, writer, []sseEvent{
			{1, eventJSON(t, 1, eventTurnFinished, map[string]any{fieldStatus: statusCompleted})},
		})
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "follow", "p1", "--from-start", "--json")
	if res.err != nil {
		t.Fatalf("follow: %v", res.err)
	}
	if calls != 2 {
		t.Fatalf("connections = %d", calls)
	}
}

func TestActivityAcceptsBothDocumentedShapes(t *testing.T) {
	prepare(t)
	payload := eventJSON(t, 1, eventTurnStarted, map[string]any{})
	shapes := map[string]string{
		"object": `{"events":[` + payload + `]}`,
		"array":  `[` + payload + `]`,
	}
	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			server := newServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(body))
			}))
			res := runCLI(t, context.Background(), "--address", server.URL, "activity", "p1", "--json")
			if res.err != nil {
				t.Fatalf("activity: %v", res.err)
			}
			var list struct {
				Events []event `json:"events"`
			}
			if err := json.Unmarshal([]byte(res.stdout), &list); err != nil {
				t.Fatalf("decode activity: %v", err)
			}
			if len(list.Events) != 1 || list.Events[0].Seq != 1 {
				t.Fatalf("events = %+v", list.Events)
			}
		})
	}
}

func TestActivityHumanOutputUsesTheTraceOrder(t *testing.T) {
	prepare(t)
	started := eventJSON(t, 1, eventToolStarted, map[string]any{fieldCallID: "c1", fieldTool: testTool})
	finished := eventJSON(t, 2, eventToolFinished, map[string]any{
		fieldCallID: "c1", fieldTool: testTool, fieldStatus: "ok", fieldDurationMS: 9,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/pages/p1/events", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"events":[` + started + `,` + finished + `]}`))
	})
	server := newServer(t, mux)
	res := runCLI(t, context.Background(), "--address", server.URL, "activity", "p1")
	if res.err != nil {
		t.Fatalf("activity: %v", res.err)
	}
	containsAll(t, res.stdout, "[1]", eventToolStarted, testTool, "[2]", "9ms")
}

func TestAPIErrorRetryability(t *testing.T) {
	if (&apiError{Status: http.StatusBadGateway}).retryable() != true {
		t.Error("a 502 should be retryable")
	}
	if (&apiError{Status: http.StatusNotFound}).retryable() != false {
		t.Error("a 404 should not be retryable")
	}
	if !errors.Is(&ExitError{Code: ExitUsage, Err: errStreamEnded}, errStreamEnded) {
		t.Error("ExitError must unwrap")
	}
}

// ensure the escaped path helper keeps a literal separator only between path
// segments.
func TestPagePathEscapesSegments(t *testing.T) {
	got := pagePath("a/b", "c d")
	want := "/api/v1/pages/a%2Fb/c%20d"
	if got != want {
		t.Fatalf("pagePath = %q, want %q", got, want)
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatalf("pagePath is not a valid URL: %v", err)
	}
}

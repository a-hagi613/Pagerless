package investigations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/internal/agentclient"
	"github.com/a-hagi613/pagerless/internal/githubpublish"
	"github.com/a-hagi613/pagerless/internal/mcp"
	"github.com/a-hagi613/pagerless/internal/transcript"
)

const (
	testOperatorToken = "operator-token"
	testInternalToken = "internal-token"
	testBaseSHA       = "base-sha"
	testBaseBranch    = "main"
	testStagingPath   = "environments/staging/app.yaml"
	testBaseContent   = "replicas: 1\n"
	testProposed      = "replicas: 2\n"

	headerAuthorization = "Authorization"
	headerOrigin        = "Origin"
	headerCookie        = "Cookie"
	headerIdempotency   = "Idempotency-Key"

	keyType        = "type"
	keyDescription = "description"
	keyKindHint    = "kind"
	testKeyObject  = "object"
	keyNamespace   = "namespace"
	keyName        = "name"
	keyInputSchema = "inputSchema"
	testNamespace  = "shop"
)

// fakeAgent is the private Python side. A scenario decides the terminal answer
// and may call back into the service.
type fakeAgent struct {
	mu       sync.Mutex
	requests []agentclient.Request
	ready    agentclient.Readiness
	scenario func(ctx context.Context, request agentclient.Request, agent *fakeAgent) agentclient.Terminal
	stops    map[string]chan struct{}
}

func (a *fakeAgent) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.URL.Path == "/readyz":
		writeJSON(response, http.StatusOK, a.ready)
	case request.URL.Path == "/turn":
		var body agentclient.Request
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.requests = append(a.requests, body)
		a.mu.Unlock()
		terminal := a.scenario(request.Context(), body, a)
		writeJSON(response, http.StatusOK, terminal)
	case strings.HasPrefix(request.URL.Path, "/turns/") && strings.HasSuffix(request.URL.Path, "/stop"):
		turnID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/turns/"), "/stop")
		a.mu.Lock()
		stop, found := a.stops[turnID]
		if !found {
			stop = make(chan struct{})
			a.stops[turnID] = stop
		}
		a.mu.Unlock()
		select {
		case <-stop:
		default:
			close(stop)
		}
		writeJSON(response, http.StatusOK, map[string]string{keyStatus: stopStopped})
	default:
		response.WriteHeader(http.StatusNotFound)
	}
}

func (a *fakeAgent) stopChannel(turnID string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stops == nil {
		a.stops = map[string]chan struct{}{}
	}
	if stop, found := a.stops[turnID]; found {
		return stop
	}
	stop := make(chan struct{})
	a.stops[turnID] = stop
	return stop
}

// fakeRepository is the in-memory GitHub repository used by the publisher.
type fakeRepository struct {
	mu          sync.Mutex
	head        string
	baseFiles   map[string]string
	branchFiles map[string]string
	branches    map[string]bool
	prs         map[string]githubpublish.PullRequest
	requests    []githubpublish.DraftRequest
	commits     int
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{
		head: testBaseSHA,
		baseFiles: map[string]string{
			testStagingPath: testBaseContent,
		},
		branchFiles: map[string]string{},
		branches:    map[string]bool{},
		prs:         map[string]githubpublish.PullRequest{},
	}
}

func (f *fakeRepository) Head(_ context.Context, ref string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == testBaseBranch {
		return f.head, true, nil
	}
	return f.head, f.branches[ref], nil
}

func (f *fakeRepository) File(_ context.Context, path, ref string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if content, found := f.branchFiles[path]; found && ref != testBaseBranch && ref != f.head {
		return []byte(content), true, nil
	}
	content, found := f.baseFiles[path]
	return []byte(content), found, nil
}

func (f *fakeRepository) CreateBranch(_ context.Context, branch, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branches[branch] {
		return false, nil
	}
	f.branches[branch] = true
	return true, nil
}

func (f *fakeRepository) PutFile(_ context.Context, branch, path, content, _ string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.branches[branch] {
		return fmt.Errorf("the branch %s does not exist", branch)
	}
	f.branchFiles[path] = content
	f.commits++
	return nil
}

func (f *fakeRepository) BlobSHA(_ context.Context, ref, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == testBaseBranch || ref == f.head {
		if _, found := f.baseFiles[path]; found {
			return "base-blob", nil
		}
		return "", nil
	}
	if _, found := f.branchFiles[path]; found {
		return "branch-blob", nil
	}
	if _, found := f.baseFiles[path]; found {
		return "base-blob", nil
	}
	return "", nil
}

func (f *fakeRepository) FindPullRequest(_ context.Context, branch string) (*githubpublish.PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, found := f.prs[branch]
	if !found {
		return nil, false, nil
	}
	return &pr, true, nil
}

func (f *fakeRepository) OpenDraftPullRequest(_ context.Context, request githubpublish.DraftRequest) (githubpublish.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	pr := githubpublish.PullRequest{Number: len(f.requests), URL: fmt.Sprintf("https://github.example/pull/%d", len(f.requests))}
	f.prs[request.Head] = pr
	return pr, nil
}

// fakeIntegration is one MCP server used by the harness.
type fakeIntegration struct {
	tools []map[string]any
	call  func(name string, arguments json.RawMessage) (string, bool)
}

func (f *fakeIntegration) ServeHTTP(response http.ResponseWriter, request *http.Request) {
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
	var result any
	switch envelope.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": mcp.ProtocolVersion}
	case "tools/list":
		result = map[string]any{"tools": f.tools}
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(envelope.Params, &call)
		text, isError := f.call(call.Name, call.Arguments)
		result = map[string]any{"isError": isError, "content": []map[string]string{{"type": "text", "text": text}}}
	default:
		writeJSON(response, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": envelope.ID,
			"error": map[string]any{"code": -32601, "message": "unknown method"},
		})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
}

type harness struct {
	t       *testing.T
	service *Service
	store   *transcript.Store
	agent   *fakeAgent
	repo    *fakeRepository
	server  *httptest.Server
	baseURL string
	client  *http.Client
}

type harnessOptions struct {
	scenario   func(ctx context.Context, request agentclient.Request, agent *fakeAgent) agentclient.Terminal
	maxActive  int
	staleBase  bool
	agentReady *agentclient.Readiness
}

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	if options.scenario == nil {
		options.scenario = func(context.Context, agentclient.Request, *fakeAgent) agentclient.Terminal {
			return agentclient.Terminal{Status: transcript.StatusCompleted, Content: "Nothing to report."}
		}
	}
	ready := agentclient.Readiness{Status: "ready", Checks: []agentclient.Check{
		{Name: CheckModel, OK: true, Reason: "a harmless tool-calling probe succeeded"},
	}}
	if options.agentReady != nil {
		ready = *options.agentReady
	}
	agent := &fakeAgent{ready: ready, scenario: options.scenario, stops: map[string]chan struct{}{}}
	agentServer := httptest.NewServer(agent)
	t.Cleanup(agentServer.Close)

	kubernetes := &fakeIntegration{
		tools: []map[string]any{
			{keyName: "resources_get", keyDescription: "read a resource", keyInputSchema: map[string]any{keyType: testKeyObject}},
			{keyName: "resources_list", keyDescription: "list resources", keyInputSchema: map[string]any{keyType: testKeyObject}},
			{keyName: "pods_log", keyDescription: "read logs", keyInputSchema: map[string]any{keyType: testKeyObject}},
		},
		call: func(name string, _ json.RawMessage) (string, bool) {
			return `{"kind":"Deployment","metadata":{"name":"checkout"},"spec":{"env":[{"name":"API_TOKEN","value":"token=abcdef123456"}]}}`, false
		},
	}
	github := &fakeIntegration{
		tools: []map[string]any{
			{keyName: "github_repository", keyDescription: "repository", keyInputSchema: map[string]any{keyType: testKeyObject}},
			{keyName: "github_get_file", keyDescription: "file", keyInputSchema: map[string]any{keyType: testKeyObject}},
			{keyName: "github_list_directory", keyDescription: "directory", keyInputSchema: map[string]any{keyType: testKeyObject}},
		},
		call: func(name string, _ json.RawMessage) (string, bool) {
			if name == "github_repository" {
				return `{"repository":"acme/cluster","defaultBranch":testBaseBranch}`, false
			}
			return `replicas: 1`, false
		},
	}
	kubernetesServer := httptest.NewServer(kubernetes)
	githubServer := httptest.NewServer(github)
	t.Cleanup(kubernetesServer.Close)
	t.Cleanup(githubServer.Close)

	repo := newFakeRepository()
	if options.staleBase {
		repo.head = "new-sha"
	}
	publisher := &githubpublish.Publisher{
		Config: githubpublish.Config{
			Repository: "acme/cluster", BaseBranch: testBaseBranch, PathPrefix: "environments/staging",
		},
		Reader: repo, Writer: repo,
	}
	token := func() (string, error) { return testInternalToken, nil }
	toolbox := mcp.NewToolbox(
		&mcp.Kubernetes{Client: mcp.New("kubernetes", kubernetesServer.URL, adaptToken(token), nil)},
		&mcp.GitHub{Client: mcp.New("github", githubServer.URL, adaptToken(token), nil), Repository: "acme/cluster"},
	)
	store, err := transcript.Open(context.Background(), transcript.Options{Path: transcript.Path(t.TempDir())})
	if err != nil {
		t.Fatalf("open the transcript: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	origin := "http://" + listener.Addr().String()

	maxActive := options.maxActive
	if maxActive == 0 {
		maxActive = 4
	}
	var service *Service
	service, err = New(context.Background(), store, Config{
		Version:        "test",
		Agent:          agentclient.New(agentServer.URL, adaptToken(token), agentServer.Client()),
		Toolbox:        toolbox,
		Publisher:      publisher,
		CallbackOrigin: origin,
		PublishCheck: func(context.Context) (string, error) {
			return "the repository is reachable; write capability is confirmed when a proposal is published", nil
		},
		Gate:           func(ctx context.Context) error { return service.ReadyGate(ctx) },
		MaxActiveTurns: maxActive,
		TurnTimeout:    10 * time.Second,
		StopGrace:      3 * time.Second,
		ToolTimeout:    3 * time.Second,
		MaxToolCalls:   4,
	})
	if err != nil {
		t.Fatalf("build the service: %v", err)
	}
	api := &API{Service: service, Auth: NewAuth(func() (string, error) { return testOperatorToken, nil }, token)}
	server := httptest.NewUnstartedServer(api.Handler())
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(func() {
		service.Close()
		server.Close()
	})
	return &harness{
		t: t, service: service, store: store, agent: agent, repo: repo,
		server: server, baseURL: server.URL, client: server.Client(),
	}
}

func adaptToken(token func() (string, error)) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token() }
}

func (h *harness) request(method, path string, body any, headers map[string]string) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode the body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, h.baseURL+path, reader)
	if err != nil {
		h.t.Fatalf("build the request: %v", err)
	}
	request.Header.Set(headerAuthorization, "Bearer "+testOperatorToken)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatalf("do the request: %v", err)
	}
	return response
}

func (h *harness) create(message string) Created {
	h.t.Helper()
	response := h.request(http.MethodPost, "/api/v1/pages",
		Request{Message: message}, map[string]string{headerIdempotency: transcript.NewID()})
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		h.t.Fatalf("create answered %d: %s", response.StatusCode, body)
	}
	var created Created
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		h.t.Fatalf("decode the created page: %v", err)
	}
	return created
}

func (h *harness) waitTurn(turnID string) transcript.Turn {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		turn, err := h.store.Turn(context.Background(), turnID)
		if err != nil {
			h.t.Fatalf("read the turn: %v", err)
		}
		if turn.Status != transcript.StatusRunning {
			return turn
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("the turn %s did not finish", turnID)
	return transcript.Turn{}
}

func (h *harness) events(pageID string, after int64) []transcript.Event {
	h.t.Helper()
	events, err := h.store.EventsAfter(context.Background(), pageID, after)
	if err != nil {
		h.t.Fatalf("read the events: %v", err)
	}
	return events
}

func decodeBody[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer func() { _ = response.Body.Close() }()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return value
}

func errorCode(t *testing.T, response *http.Response) string {
	t.Helper()
	body := decodeBody[ErrorBody](t, response)
	return body.Error.Code
}

// callback performs one private tool callback as the agent.
func callback(ctx context.Context, request agentclient.Request, callID, tool string, arguments any) (ToolCallResult, int, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return ToolCallResult{}, 0, err
	}
	body, _ := json.Marshal(ToolCallRequest{CallID: callID, Tool: tool, Arguments: encoded})
	payload := bytes.NewReader(body)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		request.CallbackURL+"/internal/turns/"+request.TurnID+"/tools", payload)
	if err != nil {
		return ToolCallResult{}, 0, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(headerAuthorization, "Bearer "+testInternalToken)
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return ToolCallResult{}, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return ToolCallResult{}, response.StatusCode, fmt.Errorf("callback answered %d: %s", response.StatusCode, raw)
	}
	var result ToolCallResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return ToolCallResult{}, response.StatusCode, err
	}
	return result, response.StatusCode, nil
}

func TestCreateAndFollowOneInvestigation(t *testing.T) {
	t.Parallel()
	scenario := func(ctx context.Context, request agentclient.Request, _ *fakeAgent) agentclient.Terminal {
		result, status, err := callback(ctx, request, "call-1", "resources_get",
			map[string]string{keyKindHint: "Deployment", keyNamespace: testNamespace, keyName: "checkout"})
		if err != nil || status != http.StatusOK {
			return agentclient.Terminal{Status: transcript.StatusFailed, Error: &agentclient.Failure{
				Code: "callback_failed", Message: fmt.Sprintf("%v %d", err, status),
			}}
		}
		if !result.Redacted {
			return agentclient.Terminal{Status: transcript.StatusFailed, Error: &agentclient.Failure{
				Code: "not_redacted", Message: "the tool result was not marked redacted",
			}}
		}
		return agentclient.Terminal{
			Status:  "completed",
			Content: "Checkout has no memory limit. The credential token=never-share-this was removed.",
			Citations: []agentclient.Citation{
				{CallID: "call-1"}, {CallID: "fabricated"},
			},
			Proposal: &agentclient.Proposal{
				BaseSHA: testBaseSHA,
				Files: []agentclient.ProposalFile{{
					Path: testStagingPath, Content: testProposed,
				}},
			},
		}
	}
	h := newHarness(t, harnessOptions{scenario: scenario})
	created := h.create("Why is checkout timing out?")
	turn := h.waitTurn(created.TurnID)
	if turn.Status != transcript.StatusCompleted {
		t.Fatalf("turn status %q (%s: %s)", turn.Status, turn.ErrorCode, turn.ErrorMessage)
	}
	page := decodeBody[struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Messages []struct {
			Role      string                `json:"role"`
			Content   string                `json:"content"`
			Citations []transcript.Citation `json:"citations"`
		} `json:"messages"`
		LastEventID int64 `json:"lastEventId"`
	}](t, h.request(http.MethodGet, "/api/v1/pages/"+created.PageID, nil, nil))
	if page.Status != transcript.StatusCompleted || len(page.Messages) != 2 {
		t.Fatalf("page %+v", page)
	}
	answer := page.Messages[1]
	if !strings.Contains(answer.Content, "[redacted:credential]") {
		t.Fatalf("the answer was not redacted: %s", answer.Content)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].CallID != "call-1" {
		t.Fatalf("citations %+v", answer.Citations)
	}
	events := h.events(created.PageID, 0)
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	want := []string{transcript.EventTurnStarted, transcript.EventToolStarted,
		transcript.EventToolFinished, transcript.EventProposal, transcript.EventAnswer,
		transcript.EventTurnFinished}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order %v, want %v", types, want)
	}
	if events[len(events)-1].Seq != page.LastEventID {
		t.Fatalf("last event id %d, stored %d", page.LastEventID, events[len(events)-1].Seq)
	}
	evidence := decodeBody[struct {
		Tool     string          `json:"tool"`
		Status   string          `json:"status"`
		Redacted bool            `json:"redacted"`
		Result   json.RawMessage `json:"result"`
	}](t, h.request(http.MethodGet, "/api/v1/pages/"+created.PageID+"/evidence/call-1", nil, nil))
	if evidence.Status != transcript.EvidenceOK || !evidence.Redacted {
		t.Fatalf("evidence %+v", evidence)
	}
	if strings.Contains(string(evidence.Result), "abcdef123456") {
		t.Fatalf("the evidence leaked a credential: %s", evidence.Result)
	}
	diff := decodeBody[struct {
		Files []transcript.FileDiff `json:"files"`
		PRURL string                `json:"prUrl"`
	}](t, h.request(http.MethodGet, "/api/v1/pages/"+created.PageID+"/diff", nil, nil))
	if len(diff.Files) != 1 || diff.PRURL == "" || !strings.Contains(diff.Files[0].Diff, "+replicas: 2") {
		t.Fatalf("diff %+v", diff)
	}
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	if len(h.repo.requests) != 1 || h.repo.commits != 1 {
		t.Fatalf("pull requests %d, commits %d", len(h.repo.requests), h.repo.commits)
	}
	if h.repo.requests[0].Base != testBaseBranch {
		t.Fatalf("the draft targets %q", h.repo.requests[0].Base)
	}
}

func TestEventsStreamReplaysAndResumesExactly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	created := h.create("Why is checkout timing out?")
	h.waitTurn(created.TurnID)

	read := func(ctx context.Context, url string, header map[string]string) []int64 {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		request.Header.Set(headerAuthorization, "Bearer "+testOperatorToken)
		request.Header.Set("Accept", "text/event-stream")
		for key, value := range header {
			request.Header.Set(key, value)
		}
		response, err := h.client.Do(request)
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream answered %d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		ids := []int64{}
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "id: ") {
				var id int64
				_, _ = fmt.Sscan(strings.TrimPrefix(line, "id: "), &id)
				ids = append(ids, id)
			}
			if strings.HasPrefix(line, "data: ") {
				var event transcript.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatalf("decode an event: %v", err)
				}
				if event.Type == transcript.EventTurnFinished {
					return ids
				}
			}
		}
		return ids
	}
	firstIDs := read(context.Background(), h.baseURL+"/api/v1/pages/"+created.PageID+"/events?after=0", nil)
	if len(firstIDs) != 3 {
		t.Fatalf("replayed %v", firstIDs)
	}
	last := firstIDs[len(firstIDs)-1]
	// A resume after the durable end stays open for new events, so the reader
	// is bounded: it must emit nothing before the deadline.
	resumeContext, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	secondIDs := read(resumeContext, h.baseURL+"/api/v1/pages/"+created.PageID+"/events",
		map[string]string{"Last-Event-ID": fmt.Sprint(last)})
	if len(secondIDs) != 0 {
		t.Fatalf("a resumed stream replayed %v", secondIDs)
	}
	seq, err := h.store.LastEventSeq(context.Background(), created.PageID)
	if err != nil || seq != last {
		t.Fatalf("last sequence %d, err %v", seq, err)
	}
}

func TestIdempotencyConflictsAndCapacity(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	h := newHarness(t, harnessOptions{
		maxActive: 1,
		scenario: func(ctx context.Context, _ agentclient.Request, _ *fakeAgent) agentclient.Terminal {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return agentclient.Terminal{Status: transcript.StatusCompleted, Content: "done"}
		},
	})
	key := "idem-key"
	request := func(key, message string) *http.Response {
		return h.request(http.MethodPost, "/api/v1/pages", Request{Message: message},
			map[string]string{headerIdempotency: key})
	}
	first := request(key, "first question")
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first create %d", first.StatusCode)
	}
	created := decodeBody[Created](t, first)
	replay := request(key, "first question")
	if replay.StatusCode != http.StatusCreated {
		t.Fatalf("replay %d", replay.StatusCode)
	}
	if replayed := decodeBody[Created](t, replay); replayed.PageID != created.PageID || replayed.TurnID != created.TurnID {
		t.Fatalf("replay returned %+v", replayed)
	}
	conflicting := request(key, "different question")
	if conflicting.StatusCode != http.StatusConflict || errorCode(t, conflicting) != "idempotency_conflict" {
		t.Fatalf("conflicting key answered %d", conflicting.StatusCode)
	}
	second := request("another-key", "second question")
	if second.StatusCode != http.StatusTooManyRequests || errorCode(t, second) != "capacity_exhausted" {
		t.Fatalf("capacity answered %d", second.StatusCode)
	}
	reply := h.request(http.MethodPost, "/api/v1/pages/"+created.PageID+"/turns",
		Request{Message: "follow up"}, map[string]string{headerIdempotency: "reply-key"})
	if reply.StatusCode != http.StatusConflict || errorCode(t, reply) != "page_busy" {
		t.Fatalf("busy page answered %d", reply.StatusCode)
	}
	sameTurn := h.request(http.MethodPost, "/api/v1/pages/"+created.PageID+"/turns",
		Request{Message: "first question"}, map[string]string{headerIdempotency: "another-key"})
	if sameTurn.StatusCode != http.StatusConflict {
		t.Fatalf("a second turn answered %d", sameTurn.StatusCode)
	}
	close(block)
	h.waitTurn(created.TurnID)
}

func TestStopIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{
		scenario: func(ctx context.Context, request agentclient.Request, agent *fakeAgent) agentclient.Terminal {
			select {
			case <-agent.stopChannel(request.TurnID):
				return agentclient.Terminal{Status: stopStopped}
			case <-ctx.Done():
				return agentclient.Terminal{Status: stopStopped}
			}
		},
	})
	created := h.create("stop me")
	stop := h.request(http.MethodPost, "/api/v1/pages/"+created.PageID+"/stop", nil, nil)
	if stop.StatusCode != http.StatusOK {
		t.Fatalf("stop answered %d", stop.StatusCode)
	}
	if status := decodeBody[struct {
		Status string `json:"status"`
	}](t, stop); status.Status != "stopped" {
		t.Fatalf("stop status %q", status.Status)
	}
	turn := h.waitTurn(created.TurnID)
	if turn.Status != transcript.StatusStopped {
		t.Fatalf("turn status %q", turn.Status)
	}
	again := h.request(http.MethodPost, "/api/v1/pages/"+created.PageID+"/stop", nil, nil)
	if again.StatusCode != http.StatusOK {
		t.Fatalf("repeated stop answered %d", again.StatusCode)
	}
	_ = again.Body.Close()
}

func TestReadinessReportsTheFailingCapability(t *testing.T) {
	t.Parallel()
	notReady := agentclient.Readiness{Status: "degraded", Checks: []agentclient.Check{
		{Name: "model", OK: false, Reason: "the model did not complete a tool call"},
	}}
	h := newHarness(t, harnessOptions{agentReady: &notReady})
	response := h.request(http.MethodGet, "/readyz", nil, nil)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz answered %d", response.StatusCode)
	}
	readiness := decodeBody[Readiness](t, response)
	if readiness.Status != "degraded" {
		t.Fatalf("status %q", readiness.Status)
	}
	found := false
	for _, check := range readiness.Checks {
		if check.Name == "model" && !check.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("checks %+v", readiness.Checks)
	}
	create := h.request(http.MethodPost, "/api/v1/pages", Request{Message: "hi"},
		map[string]string{headerIdempotency: "key"})
	if create.StatusCode != http.StatusServiceUnavailable || errorCode(t, create) != "not_ready" {
		t.Fatalf("admission answered %d", create.StatusCode)
	}
}

func TestProposalIsRejectedWhenTheBaseMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{
		staleBase: true,
		scenario: func(context.Context, agentclient.Request, *fakeAgent) agentclient.Terminal {
			return agentclient.Terminal{
				Status: transcript.StatusCompleted, Content: "A change is justified.",
				Proposal: &agentclient.Proposal{BaseSHA: testBaseSHA, Files: []agentclient.ProposalFile{{
					Path: testStagingPath, Content: testProposed,
				}}},
			}
		},
	})
	created := h.create("change this")
	if turn := h.waitTurn(created.TurnID); turn.Status != transcript.StatusCompleted {
		t.Fatalf("turn status %q", turn.Status)
	}
	diff := decodeBody[struct {
		Files []transcript.FileDiff `json:"files"`
		PRURL string                `json:"prUrl"`
	}](t, h.request(http.MethodGet, "/api/v1/pages/"+created.PageID+"/diff", nil, nil))
	if len(diff.Files) != 0 || diff.PRURL != "" {
		t.Fatalf("a stale proposal was published: %+v", diff)
	}
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	if len(h.repo.requests) != 0 {
		t.Fatal("a stale proposal opened a pull request")
	}
	events := h.events(created.PageID, 0)
	found := false
	for _, event := range events {
		if event.Type == transcript.EventProposal && strings.Contains(string(event.Data), "rejected") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rejected proposal event: %+v", events)
	}
}

func TestBrowserSessionAndOriginChecks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	origin := h.baseURL
	unauthorized := h.request(http.MethodGet, "/api/v1/pages", nil, map[string]string{headerAuthorization: ""})
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized answered %d", unauthorized.StatusCode)
	}
	_ = unauthorized.Body.Close()

	session := h.request(http.MethodPost, "/api/v1/session", map[string]string{"token": testOperatorToken},
		map[string]string{headerOrigin: origin, headerAuthorization: ""})
	if session.StatusCode != http.StatusNoContent {
		t.Fatalf("session answered %d", session.StatusCode)
	}
	cookies := session.Cookies()
	_ = session.Body.Close()
	if len(cookies) == 0 {
		t.Fatal("no session cookie was set")
	}
	var cookieHeader strings.Builder
	for index, cookie := range cookies {
		if index > 0 {
			cookieHeader.WriteString("; ")
		}
		cookieHeader.WriteString(cookie.Name + "=" + cookie.Value)
	}
	list := h.request(http.MethodGet, "/api/v1/pages", nil,
		map[string]string{headerAuthorization: "", headerCookie: cookieHeader.String()})
	if list.StatusCode != http.StatusOK {
		t.Fatalf("cookie read answered %d", list.StatusCode)
	}
	_ = list.Body.Close()
	crossOrigin := h.request(http.MethodPost, "/api/v1/pages", Request{Message: "hi"},
		map[string]string{headerAuthorization: "", headerCookie: cookieHeader.String(),
			headerIdempotency: "key", headerOrigin: "https://evil.example"})
	if crossOrigin.StatusCode != http.StatusForbidden || errorCode(t, crossOrigin) != "cross_origin_denied" {
		t.Fatalf("cross-origin write answered %d", crossOrigin.StatusCode)
	}
	sameOrigin := h.request(http.MethodPost, "/api/v1/pages", Request{Message: "hi"},
		map[string]string{headerAuthorization: "", headerCookie: cookieHeader.String(),
			headerIdempotency: "key-2", headerOrigin: origin})
	if sameOrigin.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin write answered %d", sameOrigin.StatusCode)
	}
	_ = sameOrigin.Body.Close()
}

func TestToolCallbacksAreBoundToTheTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	if _, status, _ := callback(context.Background(), agentclient.Request{
		TurnID: "missing", CallbackURL: h.baseURL,
	}, "call-1", "resources_get", map[string]string{}); status != http.StatusConflict {
		t.Fatalf("a callback for a missing turn answered %d", status)
	}

	duplicate := make(chan int, 3)
	h2 := newHarness(t, harnessOptions{
		scenario: func(ctx context.Context, request agentclient.Request, _ *fakeAgent) agentclient.Terminal {
			first, status, _ := callback(ctx, request, "call-1", "resources_get",
				map[string]string{keyKindHint: "Deployment", keyNamespace: testNamespace, keyName: "checkout"})
			if status != http.StatusOK || first.Status != transcript.EvidenceOK {
				duplicate <- status
				return agentclient.Terminal{Status: transcript.StatusFailed, Error: &agentclient.Failure{Code: "tool", Message: "tool failed"}}
			}
			_, secondStatus, _ := callback(ctx, request, "call-1", "resources_get",
				map[string]string{keyKindHint: "Service", keyNamespace: testNamespace, keyName: "other"})
			duplicate <- secondStatus
			_, deniedStatus, _ := callback(ctx, request, "call-2", "secrets_get", map[string]string{})
			duplicate <- deniedStatus
			return agentclient.Terminal{Status: transcript.StatusCompleted, Content: "done"}
		},
	})
	created := h2.create("question")
	h2.waitTurn(created.TurnID)
	if status := <-duplicate; status != http.StatusConflict {
		t.Fatalf("a reused callId answered %d", status)
	}
	if status := <-duplicate; status != http.StatusOK {
		t.Fatalf("a denied tool answered %d", status)
	}
}

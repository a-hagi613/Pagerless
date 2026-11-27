// Command fake is the integration fixture: an OpenAI-compatible Chat
// Completions endpoint that makes one read tool call and proposes a source
// change, a GitHub REST endpoint that records branches, commits and draft pull
// requests, and an API client assertion.
//
// It is built only into the integration image. It is never part of a released
// artifact and accepts any credential.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	fakeModelName = "fake-model"
	keyContent    = "content"
	keyType       = "type"
	keyName       = "name"
	keySHA        = "sha"
	keyPath       = "path"
	keyMessage    = "message"
	keyModel      = "model"
	keyObject     = "object"
	kindFile      = "file"
	notFound      = "Not Found"
	toolSubmit    = "submit_answer"

	baseSHA      = "1111111111111111111111111111111111111111"
	repository   = "acme/cluster"
	baseBranch   = "main"
	proposalPath = "apps/checkout/deployment.yaml"
	secretValue  = "token=abcdef123456"
)

// callIDPattern reads the service-assigned callId from the agent's envelope.
var callIDPattern = regexp.MustCompile(`"callId"\s*:\s*"([^"]+)"`)

const baseManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: shop
spec:
  replicas: 1
  selector:
    matchLabels:
      app: checkout
  template:
    metadata:
      labels:
        app: checkout
    spec:
      containers:
        - name: checkout
          image: registry.invalid/checkout:missing
          resources: {}
`

const proposedManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: shop
spec:
  replicas: 1
  selector:
    matchLabels:
      app: checkout
  template:
    metadata:
      labels:
        app: checkout
    spec:
      containers:
        - name: checkout
          image: registry.invalid/checkout:missing
          resources:
            limits:
              memory: 256Mi
`

func main() {
	assert := flag.Bool("assert", false, "run the API client assertion instead of serving")
	flag.Parse()
	if *assert {
		if err := runAssertion(); err != nil {
			fmt.Fprintln(os.Stderr, "fake assertion:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "fake:", err)
		os.Exit(1)
	}
}

// serve runs the model on :9090 and GitHub on :9091.
func serve() error {
	github := newGitHub()
	model := &modelServer{}
	errors := make(chan error, 2)
	go func() { errors <- http.ListenAndServe(":9090", model) }()
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/", github)
		errors <- http.ListenAndServe(":9091", mux)
	}()
	log.Println("fake model on :9090, fake GitHub on :9091")
	return <-errors
}

// modelServer answers Chat Completions. The first request asks for one read
// tool; a request that carries a tool result ends with a proposed change.
type modelServer struct{}

func (m *modelServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/models" {
		writeJSON(response, http.StatusOK, map[string]any{
			keyObject: "list", "data": []map[string]string{{"id": fakeModelName, keyObject: keyModel}},
		})
		return
	}
	if request.URL.Path != "/v1/chat/completions" && request.URL.Path != "/chat/completions" {
		http.NotFound(response, request)
		return
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(response, "bad request", http.StatusBadRequest)
		return
	}
	hasToolResult := false
	callID := ""
	for _, message := range body.Messages {
		if message.Role != "tool" {
			continue
		}
		hasToolResult = true
		var content string
		if err := json.Unmarshal(message.Content, &content); err != nil {
			content = string(message.Content)
		}
		if match := callIDPattern.FindStringSubmatch(content); len(match) == 2 {
			callID = match[1]
		}
	}
	if callID == "" {
		callID = "call-1"
	}
	available := make(map[string]bool, len(body.Tools))
	for _, tool := range body.Tools {
		available[tool.Function.Name] = true
	}
	if !hasToolResult {
		if available["resources_get"] {
			writeJSON(response, http.StatusOK, chatToolCall("resources_get",
				`{"kind":"Deployment","namespace":"shop","name":"checkout"}`))
			return
		}
		for _, tool := range body.Tools {
			if tool.Function.Name != toolSubmit && tool.Function.Name != "read_playbook" {
				writeJSON(response, http.StatusOK, chatToolCall(tool.Function.Name, `{}`))
				return
			}
		}
	}
	answer := map[string]any{
		keyContent: "The checkout Deployment has no memory limit and its image cannot be pulled. " +
			"A source change is justified.",
		"citations": []map[string]string{{"callId": callID}},
		"proposal": map[string]any{
			"baseSha": baseSHA,
			"files":   []map[string]string{{keyPath: proposalPath, keyContent: proposedManifest}},
		},
	}
	if available[toolSubmit] {
		encoded, _ := json.Marshal(answer)
		writeJSON(response, http.StatusOK, chatToolCall(toolSubmit, string(encoded)))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"id": "chatcmpl-fake", keyObject: "chat.completion", "created": time.Now().Unix(),
		keyModel: fakeModelName,
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			keyMessage: map[string]any{"role": "assistant", keyContent: answer[keyContent]},
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30},
	})

}

func chatToolCall(name, arguments string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-fake", keyObject: "chat.completion", "created": time.Now().Unix(),
		keyModel: fakeModelName,
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "tool_calls",
			keyMessage: map[string]any{
				"role": "assistant", keyContent: nil,
				"tool_calls": []map[string]any{{
					"id": "call-1", keyType: "function",
					"function": map[string]string{keyName: name, "arguments": arguments},
				}},
			},
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

// github is an in-memory repository and pull-request store.
type github struct {
	mu          sync.Mutex
	files       map[string]string
	branchFiles map[string]map[string]string
	branches    map[string]string
	pulls       []pull
	commits     int
	merges      int
}

type pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	URL    string `json:"url"`
	Draft  bool   `json:"draft"`
	Body   string `json:"body,omitempty"`
}

func newGitHub() *github {
	return &github{
		files:       map[string]string{proposalPath: baseManifest},
		branchFiles: map[string]map[string]string{},
		branches:    map[string]string{baseBranch: baseSHA},
	}
}

func (g *github) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := strings.TrimPrefix(request.URL.Path, "/repos/"+repository)
	switch {
	case request.URL.Path == "/_test/pulls":
		writeJSON(response, http.StatusOK, g.pulls)
		return
	case request.URL.Path == "/_test/merges":
		writeJSON(response, http.StatusOK, map[string]int{"merges": g.merges})
		return
	case path == "" || path == "/":
		writeJSON(response, http.StatusOK, map[string]any{
			"full_name": repository, "default_branch": baseBranch, "private": true,
			"permissions": map[string]bool{"push": true, "pull": true},
		})
		return
	case strings.HasPrefix(path, "/git/ref/heads/"):
		branch := strings.TrimPrefix(path, "/git/ref/heads/")
		sha, found := g.branches[branch]
		if !found {
			writeJSON(response, http.StatusNotFound, map[string]string{keyMessage: notFound})
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{keyObject: map[string]string{keySHA: sha}})
		return
	case path == "/git/refs" && request.Method == http.MethodPost:
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		branch := strings.TrimPrefix(body.Ref, "refs/heads/")
		if branch == "" || branch == baseBranch {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "invalid branch"})
			return
		}
		if _, found := g.branches[branch]; found {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "Reference already exists"})
			return
		}
		if body.SHA != baseSHA {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "stale base"})
			return
		}
		g.branches[branch] = baseSHA
		writeJSON(response, http.StatusCreated, map[string]string{"ref": body.Ref})
		return
	case strings.HasPrefix(path, "/contents/") || path == "/contents":
		g.contents(response, request, strings.TrimPrefix(path, "/contents/"))
		return
	case path == "/pulls" && request.Method == http.MethodGet:
		head := strings.TrimPrefix(request.URL.Query().Get("head"), "acme:")
		result := make([]map[string]any, 0, len(g.pulls))
		for _, item := range g.pulls {
			if head == "" || item.Head == head {
				result = append(result, map[string]any{
					"number": item.Number, "html_url": item.URL, "draft": item.Draft,
				})
			}
		}
		writeJSON(response, http.StatusOK, result)
		return
	case path == "/pulls" && request.Method == http.MethodPost:
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body.Head == baseBranch || body.Base != baseBranch || !body.Draft {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "refused"})
			return
		}
		number := len(g.pulls) + 1
		g.pulls = append(g.pulls, pull{
			Number: number, Title: body.Title, Head: body.Head, Base: body.Base,
			URL: fmt.Sprintf("http://pagerless-fake:9091/pull/%d", number), Draft: body.Draft, Body: body.Body,
		})
		writeJSON(response, http.StatusCreated, map[string]any{
			"number": number, "html_url": fmt.Sprintf("http://pagerless-fake:9091/pull/%d", number),
		})
		return
	case strings.HasPrefix(path, "/pulls/") && (request.Method == http.MethodPut || request.Method == http.MethodPatch):
		g.merges++
		writeJSON(response, http.StatusMethodNotAllowed, map[string]string{keyMessage: "Pagerless never merges"})
		return
	default:
		writeJSON(response, http.StatusNotFound, map[string]string{keyMessage: notFound})
	}
}

func (g *github) contents(response http.ResponseWriter, request *http.Request, path string) {
	path = strings.TrimPrefix(path, "/")
	ref := request.URL.Query().Get("ref")
	if request.Method == http.MethodPut {
		var body struct {
			Content string `json:"content"`
			Branch  string `json:"branch"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body.Branch == "" || body.Branch == baseBranch || !g.branchExists(body.Branch) {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "refused"})
			return
		}
		content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
		if err != nil {
			writeJSON(response, http.StatusUnprocessableEntity, map[string]string{keyMessage: "bad content"})
			return
		}
		if g.branchFiles[body.Branch] == nil {
			g.branchFiles[body.Branch] = map[string]string{}
		}
		g.branchFiles[body.Branch][path] = string(content)
		g.commits++
		g.branches[body.Branch] = fmt.Sprintf("commit-%d", g.commits)
		writeJSON(response, http.StatusCreated, map[string]any{"commit": map[string]string{"sha": g.branches[body.Branch]}})
		return
	}
	if strings.HasSuffix(path, "/") || path == "" {
		g.listDirectory(response, request, strings.TrimSuffix(path, "/"), ref)
		return
	}
	content, found := g.fileAt(ref, path)
	if found {
		writeJSON(response, http.StatusOK, map[string]any{
			keyType: kindFile, "encoding": "base64", "size": len(content),
			keySHA:     fmt.Sprintf("blob-%x", len(content)),
			keyContent: base64.StdEncoding.EncodeToString([]byte(content)),
		})
		return
	}
	// A directory is identified by a prefix.
	prefix := path + "/"
	entries := make([]map[string]any, 0)
	for name := range g.files {
		if trimmed, found := strings.CutPrefix(name, prefix); found {
			entries = append(entries,
				map[string]any{"name": trimmed, keyPath: name, keyType: kindFile})
		}
	}
	if len(entries) == 0 {
		writeJSON(response, http.StatusNotFound, map[string]string{keyMessage: notFound})
		return
	}
	writeJSON(response, http.StatusOK, entries)
}

func (g *github) listDirectory(response http.ResponseWriter, request *http.Request, path, ref string) {
	_ = request
	_ = ref
	entries := make([]map[string]any, 0)
	for name := range g.files {
		if path == "" || strings.HasPrefix(name, path+"/") {
			entries = append(entries, map[string]any{keyName: name, keyPath: name, keyType: kindFile})
		}
	}
	writeJSON(response, http.StatusOK, entries)
}

// fileAt returns the content at one revision. The base revision is immutable;
// a branch sees its own commits and falls back to the base before the first
// commit.
func (g *github) fileAt(ref, path string) (string, bool) {
	if ref == baseBranch || ref == baseSHA || ref == "" {
		content, found := g.files[path]
		return content, found
	}
	if content, found := g.branchFiles[ref][path]; found {
		return content, true
	}
	content, found := g.files[path]
	return content, found
}

func (g *github) branchExists(branch string) bool {
	_, found := g.branches[branch]
	return found
}

// runAssertion drives the public API as the browser chat client does.
func runAssertion() error {
	base := os.Getenv("E2E_BASE_URL")
	token := os.Getenv("E2E_TOKEN")
	question := os.Getenv("E2E_QUESTION")
	namespace := os.Getenv("E2E_NAMESPACE")
	if base == "" || token == "" || question == "" {
		return fmt.Errorf("E2E_BASE_URL, E2E_TOKEN and E2E_QUESTION are required")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	var created struct {
		PageID string `json:"pageId"`
		TurnID string `json:"turnId"`
	}
	if pageID := os.Getenv("E2E_PAGE_ID"); pageID != "" {
		// Resume the page the CLI opened. A second page would publish a second
		// proposal and make the pull-request assertion ambiguous.
		created.PageID = pageID
	} else {
		create := map[string]string{keyMessage: question}
		if namespace != "" {
			create["namespace"] = namespace
		}
		body, _ := json.Marshal(create)
		request, err := http.NewRequest(http.MethodPost, base+"/api/v1/pages", bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", randomKey())
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusCreated {
			raw, _ := io.ReadAll(response.Body)
			return fmt.Errorf("create answered %d: %s", response.StatusCode, raw)
		}
		if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
			return err
		}
	}

	events, err := follow(client, base, token, created.PageID)
	if err != nil {
		return err
	}
	types := make([]string, 0, len(events))
	var callID, prURL string
	lastSeq := int64(0)
	for index, event := range events {
		if event.Seq != lastSeq+1 && index > 0 {
			return fmt.Errorf("event sequence jumped from %d to %d", lastSeq, event.Seq)
		}
		lastSeq = event.Seq
		types = append(types, event.Type)
		var data struct {
			CallID string `json:"callId"`
			PRURL  string `json:"prUrl"`
		}
		_ = json.Unmarshal(event.Data, &data)
		if data.CallID != "" {
			callID = data.CallID
		}
		if data.PRURL != "" {
			prURL = data.PRURL
		}
	}
	if types[len(types)-1] != "turn.finished" {
		return fmt.Errorf("the stream ended with %q", types[len(types)-1])
	}
	if callID == "" {
		return fmt.Errorf("no tool call was recorded: %v", types)
	}
	if prURL == "" {
		return fmt.Errorf("no proposal was published: %v", types)
	}
	evidence, err := get(client, base, token, "/api/v1/pages/"+created.PageID+"/evidence/"+callID)
	if err != nil {
		return err
	}
	if strings.Contains(string(evidence), secretValue) || strings.Contains(string(evidence), "abcdef123456") {
		return fmt.Errorf("the evidence leaked the workload credential")
	}
	if !strings.Contains(string(evidence), "redacted") {
		return fmt.Errorf("the evidence is not marked redacted: %s", evidence)
	}
	page, err := get(client, base, token, "/api/v1/pages/"+created.PageID)
	if err != nil {
		return err
	}
	if !strings.Contains(string(page), `"role":"assistant"`) || !strings.Contains(string(page), `"completed"`) {
		return fmt.Errorf("the page has no completed answer: %s", page)
	}
	fmt.Printf("{\"pageId\":%q,\"turnId\":%q,\"events\":%d,\"lastEventId\":%d,\"prUrl\":%q}\n",
		created.PageID, created.TurnID, len(events), lastSeq, prURL)
	return nil
}

type event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// follow reads the event stream until turn.finished.
func follow(client *http.Client, base, token, pageID string) ([]event, error) {
	request, err := http.NewRequest(http.MethodGet,
		base+"/api/v1/pages/"+pageID+"/events?after=0", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("events answered %d: %s", response.StatusCode, raw)
	}
	events := make([]event, 0, 8)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var current event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &current); err != nil {
			return nil, fmt.Errorf("decode an event: %w", err)
		}
		events = append(events, current)
		if current.Type == "turn.finished" {
			return events, nil
		}
	}
	return events, fmt.Errorf("the stream ended before turn.finished: %v", scanner.Err())
}

func get(client *http.Client, base, token, path string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d: %s", path, response.StatusCode, raw)
	}
	return raw, nil
}

func randomKey() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buffer)
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

var _ = context.Background

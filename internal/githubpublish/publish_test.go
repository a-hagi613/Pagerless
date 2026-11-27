package githubpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	testRepoBase    = "base-sha"
	testRepoBranch  = "main"
	testRepoPath    = "environments/staging/app.yaml"
	testRepoUpdated = "replicas: 2\n"
)

// fakeRepo is an in-memory repository. It records every write so a test can
// prove that nothing reached the default branch and nothing merged.
type fakeRepo struct {
	mu          sync.Mutex
	base        string
	branch      string
	files       map[string]string // base revision content
	branches    map[string]string // branch -> head commit
	branchFiles map[string]map[string]string
	commits     int
	prs         map[string]PullRequest
	requests    []DraftRequest
	creates     int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		base: testRepoBase, branch: testRepoBranch,
		files: map[string]string{
			testRepoPath: "replicas: 1\n",
			"README.md":  "hello\n",
		},
		branches:    map[string]string{testRepoBranch: testRepoBase},
		branchFiles: map[string]map[string]string{testRepoBranch: {}},
		prs:         map[string]PullRequest{},
	}
}

func (f *fakeRepo) Head(_ context.Context, ref string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, found := f.branches[ref]
	return sha, found, nil
}

func (f *fakeRepo) File(_ context.Context, path, ref string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == f.base {
		content, found := f.files[path]
		return []byte(content), found, nil
	}
	content, found := f.branchFiles[ref][path]
	if !found {
		content, found = f.files[path]
	}
	return []byte(content), found, nil
}

func (f *fakeRepo) CreateBranch(_ context.Context, branch, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if _, found := f.branches[branch]; found {
		return false, nil
	}
	f.branches[branch] = sha
	f.branchFiles[branch] = map[string]string{}
	return true, nil
}

func (f *fakeRepo) PutFile(_ context.Context, branch, path, content, _ string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if branch == f.branch {
		return errors.New("a write targeted the default branch")
	}
	if f.branchFiles[branch] == nil {
		f.branchFiles[branch] = map[string]string{}
	}
	f.branchFiles[branch][path] = content
	f.commits++
	f.branches[branch] = fmt.Sprintf("commit-%d", f.commits)
	return nil
}

func (f *fakeRepo) BlobSHA(_ context.Context, ref, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref == f.base {
		if _, found := f.files[path]; found {
			return "base-blob-" + path, nil
		}
		return "", nil
	}
	if content, found := f.branchFiles[ref][path]; found {
		return fmt.Sprintf("blob-%x", len(content)), nil
	}
	if _, found := f.files[path]; found {
		return "base-blob-" + path, nil
	}
	return "", nil
}

func (f *fakeRepo) FindPullRequest(_ context.Context, branch string) (*PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, found := f.prs[branch]
	if !found {
		return nil, false, nil
	}
	return &pr, true, nil
}

func (f *fakeRepo) OpenDraftPullRequest(_ context.Context, request DraftRequest) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	pr := PullRequest{Number: len(f.requests), URL: fmt.Sprintf("https://github.example/pull/%d", len(f.requests))}
	f.prs[request.Head] = pr
	return pr, nil
}

func testPublisher(repo *fakeRepo) *Publisher {
	return &Publisher{
		Config: Config{Repository: "acme/cluster", BaseBranch: testRepoBranch, PathPrefix: "environments/staging"},
		Reader: repo,
		Writer: repo,
	}
}

func testProposal() Proposal {
	return Proposal{
		PageID: "page-1", TurnID: "turn-1", Question: "Why is checkout timing out?",
		Findings: "The Deployment has no memory limit.", BaseSHA: testRepoBase,
		Evidence: []EvidenceRef{{CallID: "c1", Tool: "resources_get"}},
		Files:    []File{{Path: testRepoPath, Content: testRepoUpdated}},
	}
}

func TestPublishOpensOneDraftPullRequest(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	published, err := testPublisher(repo).Publish(context.Background(), testProposal())
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.PullRequest.URL == "" || !published.PullRequest.Created {
		t.Fatalf("published %+v", published)
	}
	if len(repo.requests) != 1 {
		t.Fatalf("opened %d pull requests", len(repo.requests))
	}
	request := repo.requests[0]
	if request.Base != testRepoBranch || request.Head == testRepoBranch {
		t.Fatalf("the pull request targets %+v", request)
	}
	expectedFields := []string{
		"Why is checkout timing out?", "The Deployment has no memory limit.", "`c1`", testRepoPath,
	}
	for _, expected := range expectedFields {
		if !strings.Contains(request.Body, expected) {
			t.Fatalf("the body is missing %q:\n%s", expected, request.Body)
		}
	}
	if len(published.Diff) != 1 || !strings.Contains(published.Diff[0].Diff, "-replicas: 1") {
		t.Fatalf("diff %+v", published.Diff)
	}
}

func TestPublishRefusesAStaleBase(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	proposal := testProposal()
	proposal.BaseSHA = "older-sha"
	if _, err := testPublisher(repo).Publish(context.Background(), proposal); !errors.Is(err, ErrBase) {
		t.Fatalf("stale base error %v", err)
	}
	if len(repo.requests) != 0 || repo.commits != 0 {
		t.Fatal("a stale proposal wrote to the repository")
	}
}

func TestPublishRefusesAPathOutsideThePrefix(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	for _, path := range []string{
		"../secret.yaml", "/etc/passwd", "environments/prod/app.yaml", `environments\staging\app.yaml`,
	} {
		proposal := testProposal()
		proposal.Files = []File{{Path: path, Content: testRepoUpdated}}
		if _, err := testPublisher(repo).Publish(context.Background(), proposal); !errors.Is(err, ErrPath) {
			t.Fatalf("%q: %v", path, err)
		}
	}
	if repo.commits != 0 {
		t.Fatal("a refused path was committed")
	}
}

func TestPublishRecoversAnExistingPullRequest(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	publisher := testPublisher(repo)
	first, err := publisher.Publish(context.Background(), testProposal())
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	second, err := publisher.Publish(context.Background(), testProposal())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.PullRequest.URL != first.PullRequest.URL || second.PullRequest.Created {
		t.Fatalf("retry created another pull request: %+v", second)
	}
	if len(repo.requests) != 1 {
		t.Fatalf("opened %d pull requests", len(repo.requests))
	}
	if repo.commits != 1 {
		t.Fatalf("committed %d times", repo.commits)
	}
}

func TestPublishRecoversABranchWithoutAPullRequest(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	proposal := testProposal()
	branch := branchName(proposal)
	if _, err := repo.CreateBranch(context.Background(), branch, testRepoBase); err != nil {
		t.Fatalf("create the branch: %v", err)
	}
	if err := repo.PutFile(context.Background(), branch, testRepoPath,
		testRepoUpdated, "message", "base-blob"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	published, err := testPublisher(repo).Publish(context.Background(), proposal)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if published.PullRequest.URL == "" {
		t.Fatal("the retry did not open a pull request")
	}
	if repo.commits != 1 {
		t.Fatalf("the retry duplicated the commit: %d", repo.commits)
	}
}

func TestPublishRefusesAnUnchangedFile(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	proposal := testProposal()
	proposal.Files = []File{{Path: "README.md", Content: "hello\n"}}
	// README.md is outside the staging prefix, so use the allowed file.
	proposal.Files = []File{{Path: testRepoPath, Content: "replicas: 1\n"}}
	if _, err := testPublisher(repo).Publish(context.Background(), proposal); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("unchanged proposal error %v", err)
	}
}

// TestRESTUsesTheReadAndWriteCredentialsSeparately checks that the read calls
// carry the read token and the branch and pull-request calls carry the write
// token, and that the pull request is a draft.
func TestRESTUsesTheReadAndWriteCredentialsSeparately(t *testing.T) {
	t.Parallel()
	var (
		mu        sync.Mutex
		readSeen  int
		writeSeen int
		draft     bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/git/ref/heads/main"):
			readSeen++
			_, _ = fmt.Fprintf(response, `{"object":{"sha":%q}}`, testRepoBase)
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/git/ref/heads/"):
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"message":"Not Found"}`))
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/contents/"):
			readSeen++
			_, _ = response.Write([]byte(`{"type":"file","encoding":"base64","content":"cmVwbGljYXM6IDEK"}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/pulls"):
			writeSeen++
			_, _ = response.Write([]byte(`[]`))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/git/refs"):
			writeSeen++
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{}`))
		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/contents/"):
			writeSeen++
			if token != "write-token" {
				t.Errorf("a write used %q", token)
			}
			_, _ = response.Write([]byte(`{}`))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/pulls"):
			writeSeen++
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			draft, _ = body["draft"].(bool)
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"number":7,"html_url":"https://github.example/pull/7"}`))
		default:
			t.Errorf("unexpected %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	readToken := func(context.Context) (string, error) { return "read-token", nil }
	read, err := NewREST(server.URL, "acme/cluster", readToken, server.Client())
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	writeToken := func(context.Context) (string, error) { return "write-token", nil }
	write, err := NewREST(server.URL, "acme/cluster", writeToken, server.Client())
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	publisher := &Publisher{
		Config: Config{Repository: "acme/cluster", BaseBranch: testRepoBranch},
		Reader: read, Writer: write,
	}
	proposal := testProposal()
	proposal.Files = []File{{Path: "app.yaml", Content: testRepoUpdated}}
	published, err := publisher.Publish(context.Background(), proposal)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.PullRequest.URL == "" || !draft {
		t.Fatalf("published %+v, draft=%v", published, draft)
	}
	mu.Lock()
	defer mu.Unlock()
	if readSeen == 0 || writeSeen == 0 {
		t.Fatalf("read calls %d, write calls %d", readSeen, writeSeen)
	}
}

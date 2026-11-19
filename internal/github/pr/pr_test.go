package pr_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/internal/github/pr"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

const (
	// proposalHash is a lowercase hexadecimal SHA-256 digest.
	proposalHash = "abababababababababababababababababababababababababababababababab"
	// otherHash is a second Proposal hash.
	otherHash = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// approved returns an Approved value against the harness's repository.
func approved(t *testing.T, repo *testenv.BareRepo, hash, content string) pr.Approved {
	t.Helper()
	return pr.Approved{
		Page:       pr.PageRef{Namespace: "pagerless", Name: "checkout", UID: "uid-1"},
		Hash:       hash,
		BaseBranch: testenv.SandboxBranch,
		BaseCommit: repo.Head(),
		Files: []pr.File{{
			Path: "apps/checkout/deployment.yaml", Content: []byte(content), Digest: digest(content),
		}},
		Title: "Pagerless: recover shop/checkout",
		Body:  "What failed: checkout crash-loops.\n\nRoot cause: the memory limit is too low.",
	}
}

// digest returns the SHA-256 digest of the content.
func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// newClient returns a writer for the fake GitHub.
func newClient(t *testing.T, repo *testenv.BareRepo) (*pr.Client, *testenv.FakeGitHub) {
	t.Helper()
	fake := testenv.NewFakeGitHub(t, repo)
	return &pr.Client{
		APIURL:     fake.URL,
		Repository: testenv.GitHubRepository,
		Token:      func() (string, error) { return fake.Token(), nil },
	}, fake
}

func TestEnsureCreatesOneBranchAndOnePullRequest(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, fake := newClient(t, repo)
	want := approved(t, repo, proposalHash, "kind: Deployment\n")

	result, err := client.Ensure(context.Background(), want)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if want := "pagerless/checkout-abababab"; result.Branch != want {
		t.Errorf("branch = %q, want %q", result.Branch, want)
	}
	if !result.Created || result.Number != 1 || result.URL == "" {
		t.Errorf("result = %+v, want one created Pull Request", result)
	}
	if got := repo.Rev("refs/heads/" + result.Branch); got != result.HeadSHA {
		t.Errorf("branch head = %s, result head = %s", got, result.HeadSHA)
	}
	if got := repo.File(result.Branch, "apps/checkout/deployment.yaml"); got != "kind: Deployment\n" {
		t.Errorf("committed file = %q, want the approved content", got)
	}
	pullRequests := fake.PullRequests()
	if len(pullRequests) != 1 || pullRequests[0].Draft {
		t.Fatalf("pull requests = %+v, want one non-draft Pull Request", pullRequests)
	}
	if !strings.Contains(repo.Message(result.Branch), "Pagerless-Proposal-Hash: "+proposalHash) {
		t.Errorf("commit message = %q, want the Proposal hash", repo.Message(result.Branch))
	}
	if len(fake.GraphQLRequests()) != 0 {
		t.Errorf("GraphQL requests = %v, want none: Pagerless never enables auto-merge", fake.GraphQLRequests())
	}
	// A rerun after a crash reuses the branch and the Pull Request.
	again, err := client.Ensure(context.Background(), want)
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if again.Created || again.Number != result.Number || again.Branch != result.Branch || again.HeadSHA != result.HeadSHA {
		t.Errorf("second Ensure = %+v, want the first result without creating anything", again)
	}
	if len(fake.PullRequests()) != 1 {
		t.Errorf("pull requests = %d after a rerun, want 1", len(fake.PullRequests()))
	}
}

func TestEnsureReusesTheExistingBranchAndPullRequest(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, fake := newClient(t, repo)
	want := approved(t, repo, proposalHash, "kind: Deployment\n")
	first, err := client.Ensure(context.Background(), want)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A crash after the branch and the Pull Request exist must not create a
	// second one. Closing the Pull Request keeps its branch, which is what the
	// writer recovers from.
	fake.Close(t, first.Number)
	second, err := client.Ensure(context.Background(), want)
	if err != nil {
		t.Fatalf("Ensure after a crash: %v", err)
	}
	if second.Number != first.Number || second.HeadSHA != first.HeadSHA {
		t.Errorf("second Ensure = %+v, want the existing Pull Request and commit", second)
	}
	if got := repo.Rev("refs/heads/" + first.Branch); got != first.HeadSHA {
		t.Errorf("branch head = %s, want the existing commit %s", got, first.HeadSHA)
	}
}

func TestEnsureRefusesAConflictingBranch(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, _ := newClient(t, repo)
	want := approved(t, repo, proposalHash, "kind: Deployment\n")
	first, err := client.Ensure(context.Background(), want)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	conflict := want
	conflict.Files[0].Content = []byte("kind: Deployment\nchanged: true\n")
	conflict.Files[0].Digest = digest(string(conflict.Files[0].Content))
	_, err = client.Ensure(context.Background(), conflict)
	if !errors.Is(err, pr.ErrBranchConflict) {
		t.Fatalf("Ensure = %v, want ErrBranchConflict", err)
	}
	if got := repo.File(first.Branch, "apps/checkout/deployment.yaml"); got != "kind: Deployment\n" {
		t.Errorf("the conflicting branch holds %q, want the original content", got)
	}
}

func TestEnsureRefusesAStaleBase(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, _ := newClient(t, repo)
	want := approved(t, repo, proposalHash, "kind: Deployment\n")
	repo.Commit("README.md", "changed\n", "Advance main")
	_, err := client.Ensure(context.Background(), want)
	if !errors.Is(err, pr.ErrBaseMoved) {
		t.Fatalf("Ensure = %v, want ErrBaseMoved", err)
	}
}

func TestEnsureSupersedesThePreviousPullRequest(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, fake := newClient(t, repo)
	first := approved(t, repo, proposalHash, "kind: Deployment\n")
	opened, err := client.Ensure(context.Background(), first)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	second := approved(t, repo, otherHash, "kind: Deployment\n# revised\n")
	second.Supersedes = opened.Number
	revision, err := client.Ensure(context.Background(), second)
	if err != nil {
		t.Fatalf("Ensure the revision: %v", err)
	}
	if revision.Number == opened.Number {
		t.Fatalf("the revision reused Pull Request %d", revision.Number)
	}
	pullRequests := fake.PullRequests()
	if len(pullRequests) != 2 || pullRequests[0].State != testenv.PRClosed {
		t.Fatalf("pull requests = %+v, want the first closed", pullRequests)
	}
	comments := fake.Comments(opened.Number)
	if len(comments) != 1 || !strings.Contains(comments[0], revision.URL) {
		t.Errorf("comments = %v, want a link to %s", comments, revision.URL)
	}
}

func TestEnsureReportsAPermissionRefusal(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, fake := newClient(t, repo)
	fake.FailNext("POST", "/git/blobs", 403, 1)
	_, err := client.Ensure(context.Background(), approved(t, repo, proposalHash, "kind: Deployment\n"))
	if !errors.Is(err, pr.ErrPermissionDenied) {
		t.Fatalf("Ensure = %v, want ErrPermissionDenied", err)
	}
}

func TestEnsureValidatesItsInput(t *testing.T) {
	t.Parallel()
	repo := testenv.NewBareRepo(t)
	client, _ := newClient(t, repo)
	cases := map[string]func(*pr.Approved){
		"no files":      func(a *pr.Approved) { a.Files = nil },
		"bad hash":      func(a *pr.Approved) { a.Hash = "not-a-hash" },
		"bad digest":    func(a *pr.Approved) { a.Files[0].Digest = strings.Repeat("0", 64) },
		"empty title":   func(a *pr.Approved) { a.Title = " " },
		"two same file": func(a *pr.Approved) { a.Files = append(a.Files, a.Files[0]) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := approved(t, repo, proposalHash, "kind: Deployment\n")
			mutate(&want)
			if _, err := client.Ensure(context.Background(), want); err == nil {
				t.Fatal("Ensure accepted an invalid Approved value")
			}
		})
	}
}

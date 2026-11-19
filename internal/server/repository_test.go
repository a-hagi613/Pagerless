package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/credentials"
	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// TestTheCredentialsSecretConfiguresThePullRequestPath starts the server with
// no explicit Target Repository, seeds GITHUB_REPO in the credentials Secret,
// and proves that an approved FilePatch Proposal reaches the GitHub writer.
func TestTheCredentialsSecretConfiguresThePullRequestPath(t *testing.T) {
	env := testenv.Start(t,
		testenv.WithTargetRepositoryFromSecret(),
		testenv.WithCredentials(func(e *testenv.Env) map[string]string {
			return map[string]string{
				credentials.KeyGitHubRepo:   testenv.GitHubRepository,
				credentials.KeyGitHubAPIURL: e.GitHub.URL,
				credentials.KeyGitHubToken:  testenv.GitHubWriteToken,
			}
		}),
	)

	created := env.CreatePage(t, testenv.NewPage(checkout))
	page := env.WaitForPhase(t, created.Name, v1alpha1.PhaseInvestigating)
	if page.Status.Repository == nil || page.Status.Repository.URL == "" {
		t.Fatalf("the Page has no Target Repository from the credentials Secret: %+v", page.Status.Repository)
	}
	if !strings.Contains(page.Status.Repository.URL, testenv.GitHubRepository) {
		t.Errorf("repository URL = %q, want it to name %s", page.Status.Repository.URL, testenv.GitHubRepository)
	}

	content := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: checkout\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	hash := strings.Repeat("a", 64)
	if err := env.History.SaveProposalContents(t.Context(), hash, map[string][]byte{
		"deployment.yaml": content,
	}, time.Now()); err != nil {
		t.Fatalf("save the Proposal content: %v", err)
	}

	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	asServer, err := client.New(env.ServerConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var latest v1alpha1.Page
	if err := asServer.Get(t.Context(), client.ObjectKeyFromObject(page), &latest); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now())
	proposal := &v1alpha1.Proposal{
		Kind: v1alpha1.ProposalKindFilePatch,
		Base: &v1alpha1.ProposalBase{
			Commit: env.Sandbox.Head(), TargetGeneration: latest.Generation,
		},
		Hash: hash, Attempt: 1, ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	proposal.ProposedAction = v1alpha1.ProposedAction{
		Action: v1alpha1.ActionFilePatch,
		Parameters: &v1alpha1.ActionParameters{FilePatch: &v1alpha1.FilePatchParameters{
			Files: []v1alpha1.FileEdit{{
				Path:          "deployment.yaml",
				BaseBlob:      strings.Repeat("b", 40),
				ContentDigest: digest,
				Rationale:     "the manifest that owns the Target",
			}},
			Confidence: "exact",
		}},
	}
	latest.Status.Proposal = proposal
	latest.Status.Decision = &v1alpha1.Decision{
		Outcome: v1alpha1.DecisionApproved, ProposalHash: hash,
		DecidedBy: "alice@example.com", DecidedAt: now,
	}
	latest.Status.Phase = v1alpha1.PhaseApproved
	if err := asServer.Status().Update(t.Context(), &latest); err != nil {
		t.Fatalf("record the approved FilePatch Proposal: %v", err)
	}

	env.WaitFor(t, created.Name, "the pull request is recorded", func(p *v1alpha1.Page) bool {
		return p.Status.PullRequest != nil && p.Status.PullRequest.URL != ""
	})
	pullRequests := env.GitHub.PullRequests()
	if len(pullRequests) != 1 {
		t.Fatalf("pull requests = %d, want the one the approved Proposal opened", len(pullRequests))
	}
	if pullRequests[0].Base != "main" {
		t.Errorf("pull request base = %q, want main", pullRequests[0].Base)
	}
	if !strings.Contains(pullRequests[0].Title, "checkout") {
		t.Errorf("pull request title = %q, want the Page's Target", pullRequests[0].Title)
	}
}

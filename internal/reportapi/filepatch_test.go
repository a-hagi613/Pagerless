package reportapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/tools"
)

// fakeBlobs serves one file at a fixed blob SHA.
type fakeBlobs struct {
	sha     string
	missing bool
}

func (f fakeBlobs) Tree(context.Context, tools.Repository, string, string) (tools.Tree, error) {
	return tools.Tree{}, errors.New("unused")
}

func (f fakeBlobs) File(
	_ context.Context, _ tools.Repository, _, path string,
) (tools.File, error) {
	if f.missing {
		return tools.File{}, errors.New("not found")
	}
	return tools.File{Path: path, SHA: f.sha, Content: "kind: Deployment\n"}, nil
}

func (f fakeBlobs) Commits(
	context.Context, tools.Repository, string, string, int,
) ([]tools.Commit, error) {
	return nil, errors.New("unused")
}

// filePatchPage is a Page with a declared Target Repository.
func filePatchPage() *v1alpha1.Page {
	page := &v1alpha1.Page{}
	page.Name, page.Namespace = "checkout-oom", "pagerless"
	page.Spec.Repository = &v1alpha1.RepositoryReference{
		URL: "https://github.com/acme/gitops", Branch: "main",
	}
	return page
}

// filePatchAction is one whole-file edit.
func filePatchAction(path, content string) v1alpha1.ProposedAction {
	return v1alpha1.ProposedAction{
		Action: v1alpha1.ActionFilePatch,
		Parameters: &v1alpha1.ActionParameters{FilePatch: &v1alpha1.FilePatchParameters{
			Files: []v1alpha1.FileEdit{{Path: path, UpdatedContent: content, Rationale: "the cause"}},
		}},
	}
}

// TestPrepareFilePatchRecordsTheBaseBlobAndDigest covers the server half of a
// FilePatch: the base blob comes from the repository, the content digest is
// computed, and the raw content leaves the Page.
func TestPrepareFilePatchRecordsTheBaseBlobAndDigest(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	handler := &Handler{Blobs: fakeBlobs{sha: sha}}
	content := "kind: Deployment\nmetadata:\n  name: checkout\n"
	action, contents, errs := handler.prepareFilePatch(context.Background(), filePatchPage(),
		filePatchAction("apps/shop/checkout.yaml", content))
	if len(errs) != 0 {
		t.Fatalf("errors = %+v, want the FilePatch prepared", errs)
	}
	file := action.Parameters.FilePatch.Files[0]
	if file.BaseBlob != sha {
		t.Errorf("base blob = %q, want %q", file.BaseBlob, sha)
	}
	sum := sha256.Sum256([]byte(content))
	if file.ContentDigest != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %q, want the digest of the content", file.ContentDigest)
	}
	if file.UpdatedContent != "" {
		t.Errorf("updated content = %q, want it cleared from the Page", file.UpdatedContent)
	}
	if string(contents["apps/shop/checkout.yaml"]) != content {
		t.Errorf("contents = %q, want the complete file", contents)
	}
}

// TestPrepareFilePatchRefusesWhatTheRepositoryDoesNotHave keeps a Proposal from
// naming a file that does not exist.
func TestPrepareFilePatchRefusesWhatTheRepositoryDoesNotHave(t *testing.T) {
	t.Parallel()
	handler := &Handler{Blobs: fakeBlobs{missing: true}}
	_, _, errs := handler.prepareFilePatch(context.Background(), filePatchPage(),
		filePatchAction("apps/shop/missing.yaml", "kind: Deployment\n"))
	if len(errs) != 1 || errs[0].Field != "proposal.files[0].path" {
		t.Errorf("errors = %+v, want proposal.files[0].path", errs)
	}
}

// TestPrepareFilePatchNeedsATargetRepository keeps an ungrounded Proposal from
// being stored.
func TestPrepareFilePatchNeedsATargetRepository(t *testing.T) {
	t.Parallel()
	handler := &Handler{Blobs: fakeBlobs{sha: strings.Repeat("a", 40)}}
	page := filePatchPage()
	page.Spec.Repository = nil
	_, _, errs := handler.prepareFilePatch(context.Background(), page,
		filePatchAction("apps/shop/checkout.yaml", "kind: Deployment\n"))
	if len(errs) != 1 || errs[0].Field != "proposal.kind" {
		t.Errorf("errors = %+v, want proposal.kind", errs)
	}
}

package proposal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// hashDomainV2 separates the v2 Proposal hash from every other SHA-256 use and
// versions the encoding below. A v2 hash and a v1 hash of the same Page can
// never collide, and a future encoding change gets a new domain.
const hashDomainV2 = "pagerless.dev/proposal/v2\n"

// HashInput is everything that the v2 Proposal hash covers. The model name and
// the token counts are deliberately absent: they describe how the Proposal was
// produced, not what a human approves.
type HashInput struct {
	// Page is the Page's identity.
	Page PageIdentity `json:"page"`
	// Target identifies the failing object.
	Target v1alpha1.TargetReference `json:"target"`
	// TargetGeneration is the Target's metadata.generation when the Proposal was
	// rendered. A moved generation is a different Proposal.
	TargetGeneration int64 `json:"targetGeneration"`
	// Kind is the Proposal's form.
	Kind v1alpha1.ProposalKind `json:"kind"`
	// DiagnosisDigest is the lowercase hexadecimal SHA-256 digest of the Diagnosis.
	DiagnosisDigest string `json:"diagnosisDigest"`
	// Repository is the Target Repository's URL.
	Repository string `json:"repository"`
	// BaseCommit is the commit the Proposal was rendered against.
	BaseCommit string `json:"baseCommit"`
	// Resolution is the provenance ladder's recorded result.
	Resolution ResolutionInput `json:"resolution"`
	// Files are the whole-file changes.
	Files []FileInput `json:"files"`
}

// PageIdentity is the part of a Page that identifies it.
type PageIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// ResolutionInput is the part of a Resolution that a Proposal hash covers. The
// resolution time is excluded: two resolutions that record the same facts are
// the same resolution.
type ResolutionInput struct {
	Source         v1alpha1.ResolutionSource      `json:"source"`
	Confidence     v1alpha1.Confidence            `json:"confidence"`
	URL            string                         `json:"url"`
	Branch         string                         `json:"branch"`
	Path           string                         `json:"path"`
	EditTarget     string                         `json:"editTarget"`
	EditTargetKind v1alpha1.EditTargetKind        `json:"editTargetKind"`
	Ambiguous      bool                           `json:"ambiguous"`
	Candidates     []v1alpha1.ResolutionCandidate `json:"candidates"`
	Attempts       []v1alpha1.ResolutionAttempt   `json:"attempts"`
	Reason         string                         `json:"reason"`
}

// FileInput is one file's contribution to the hash: its path, base blob,
// content digest, and rationale. The content itself is covered by the digest.
type FileInput struct {
	Path          string `json:"path"`
	BaseBlob      string `json:"baseBlob"`
	ContentDigest string `json:"contentDigest"`
	Rationale     string `json:"rationale"`
}

// HashFileProposal returns the hash that an Approval of a v2 Proposal binds to:
// a lowercase hexadecimal SHA-256 digest over the domain separator and the
// canonical JSON encoding of the input.
func HashFileProposal(input HashInput) (string, error) {
	if input.Page.Name == "" || input.Page.Namespace == "" {
		return "", errors.New("the Page identity is incomplete")
	}
	if input.Kind == "" {
		return "", errors.New("the Proposal kind is empty")
	}
	if input.BaseCommit == "" {
		return "", errors.New("the base commit is empty")
	}
	if len(input.Files) == 0 {
		return "", errors.New("the Proposal changes no files")
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode the Proposal for hashing: %w", err)
	}
	sum := sha256.Sum256(append([]byte(hashDomainV2), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

// HashInputFor derives the hash input from a Page. A Page without a Proposal
// or a Resolution has nothing to hash.
func HashInputFor(page *v1alpha1.Page) (HashInput, error) {
	current := page.Status.Proposal
	if current == nil {
		return HashInput{}, errors.New("the Page has no Proposal")
	}
	resolution := page.Status.Repository
	if resolution == nil {
		return HashInput{}, errors.New("the Page has no Resolution")
	}
	if current.Base == nil {
		return HashInput{}, errors.New("the Proposal has no base")
	}
	if len(current.Files) == 0 {
		return HashInput{}, errors.New("the Proposal changes no files")
	}
	sum := sha256.Sum256([]byte(page.Status.Diagnosis))
	files := make([]FileInput, 0, len(current.Files))
	for _, file := range current.Files {
		files = append(files, FileInput{
			Path: file.Path, BaseBlob: file.BaseBlob, ContentDigest: file.ContentDigest,
			Rationale: file.Rationale,
		})
	}
	return HashInput{
		Page:             PageIdentity{Namespace: page.Namespace, Name: page.Name, UID: string(page.UID)},
		Target:           page.Spec.Target,
		TargetGeneration: current.Base.TargetGeneration,
		Kind:             current.Kind,
		DiagnosisDigest:  hex.EncodeToString(sum[:]),
		Repository:       resolution.URL,
		BaseCommit:       current.Base.Commit,
		Resolution: ResolutionInput{
			Source: resolution.Source, Confidence: resolution.Confidence,
			URL: resolution.URL, Branch: resolution.Branch, Path: resolution.Path,
			EditTarget: resolution.EditTarget, EditTargetKind: resolution.EditTargetKind,
			Ambiguous: resolution.Ambiguous, Candidates: resolution.Candidates,
			Attempts: resolution.Attempts, Reason: resolution.Reason,
		},
		Files: files,
	}, nil
}

// ProposalHash returns the v2 hash of the Page's current Proposal and records
// it on the Proposal.
func ProposalHash(page *v1alpha1.Page) (string, error) {
	input, err := HashInputFor(page)
	if err != nil {
		return "", err
	}
	return HashFileProposal(input)
}

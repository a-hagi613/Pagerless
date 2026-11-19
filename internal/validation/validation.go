// Package validation is the deterministic, read-only Proposal Validation
// (ADR-0042). It runs cheap checks first and expensive ones last:
//
//  1. the file count, path, size, digest, and base-blob bounds;
//  2. the path policy;
//  3. strict YAML parsing with document, node, and depth budgets;
//  4. the kind allowlist and its always-denied set;
//  5. identity preservation against the base documents;
//  6. the forbidden differences;
//  7. the API server's own OpenAPI v3 schemas, cached per group and version,
//     which degrade the Proposal rather than blocking it when unavailable;
//  8. the chart's values.schema.json for a Helm values edit;
//  9. the derived Diff bounds.
//
// It never calls a mutating verb and never uses a dry run. Admission, quota,
// and defaulting are explicitly not evaluated, because the Target Repository's
// own Checks are the validating authority.
package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// MaxFiles is the number of files that one Proposal may change.
	MaxFiles = v1alpha1.MaxProposalFiles
	// MaxFileBytes is the largest file that one Proposal may carry.
	MaxFileBytes = v1alpha1.MaxFileContentBytes
	// MaxDiffBytes is the largest derived Diff that one Proposal may produce.
	MaxDiffBytes = 32 << 10
	// MaxChangedLines is the largest number of changed lines in one Proposal.
	MaxChangedLines = 400
	// MaxReplicas is the largest replica count that Validation accepts.
	MaxReplicas = 10
)

// Blob is one file at the base commit.
type Blob struct {
	// SHA is the Git blob SHA.
	SHA string
	// Content is the file's bytes.
	Content []byte
}

// Repository reads the base commit that a Proposal is rendered against.
type Repository interface {
	// File returns the blob at the commit, or an error when the file does not
	// exist there.
	File(ctx context.Context, commit, filePath string) (Blob, error)
}

// Schemas validates one single-resource document against the API server's own
// OpenAPI v3 schema. An implementation caches per group and version.
type Schemas interface {
	// Validate returns the ways the object differs from its schema: an unknown
	// field, a missing required field, or a wrong type. It returns
	// ErrSchemaUnavailable when no schema covers the object.
	Validate(ctx context.Context, apiVersion, kind string, object map[string]any) ([]string, error)
}

// ErrSchemaUnavailable means no OpenAPI schema covers an object, so schema
// validation degrades rather than blocking the Proposal.
var ErrSchemaUnavailable = errors.New("no OpenAPI schema is available for the object")

// ValuesSchemas returns the values.schema.json that belongs to a Helm values file.
type ValuesSchemas interface {
	// Schema returns the chart's values schema, or nil when the chart has none.
	Schema(ctx context.Context, commit, valuesPath string) ([]byte, error)
}

// Request is one Proposal to validate.
type Request struct {
	// BaseCommit is the commit the Proposal was rendered against.
	BaseCommit string
	// Files are the whole-file changes.
	Files []v1alpha1.FileEdit
	// Contents are the complete new contents, keyed by path. They are held by
	// the server rather than the Page, and each must match its file's digest.
	Contents map[string][]byte
	// EditTarget is the resolved edit target. Validation uses it to recognise a
	// Helm values edit.
	EditTarget string
	// Repository reads the base commit.
	Repository Repository
	// ValuesSchemas returns values schemas. Nil skips the Helm values check.
	ValuesSchemas ValuesSchemas
}

// FileDiff is the Diff that Validation derived for one file.
type FileDiff struct {
	// Path is the file.
	Path string
	// Diff is the unified diff from the base content to the proposed content.
	Diff string
	// Added and Removed count the changed lines.
	Added, Removed int
}

// Result is the outcome of Validation.
type Result struct {
	// Passed reports whether the Proposal is acceptable.
	Passed bool
	// Degraded reports that schema validation could not run.
	Degraded bool
	// Errors are the refusals.
	Errors []v1alpha1.ProposalValidationError
	// Warnings are permitted changes that the Operator must review.
	Warnings []v1alpha1.ProposalWarning
	// Diffs are the derived diffs, one per file.
	Diffs []FileDiff
}

// Validation returns the report that the Page records.
func (r Result) Validation() *v1alpha1.ProposalValidation {
	return &v1alpha1.ProposalValidation{
		Passed:    r.Passed,
		Degraded:  r.Degraded,
		Errors:    r.Errors,
		Warnings:  r.Warnings,
		Admission: v1alpha1.AdmissionNotEvaluated,
	}
}

// refuse adds an error and marks the Proposal refused.
func (r *Result) refuse(field, format string, args ...any) {
	r.Passed = false
	if len(r.Errors) < 10 {
		r.Errors = append(r.Errors, v1alpha1.ProposalValidationError{
			Field: field, Message: fmt.Sprintf(format, args...),
		})
	}
}

// warn records a permitted change.
func (r *Result) warn(class v1alpha1.ProposalWarningClass, field, format string, args ...any) {
	if len(r.Warnings) >= 10 {
		return
	}
	r.Warnings = append(r.Warnings, v1alpha1.ProposalWarning{
		Class: class, Field: field, Message: fmt.Sprintf(format, args...),
	})
}

// Validator validates Proposals.
type Validator struct {
	schemas Schemas
}

// New returns a Validator that uses the schemas. A nil Schemas degrades every
// Proposal that carries a document, rather than refusing it.
func New(schemas Schemas) *Validator { return &Validator{schemas: schemas} }

// document is one parsed Kubernetes document with its index in the file.
type document struct {
	index int
	value map[string]any
	// apiVersion, kind, namespace, and name identify the document.
	apiVersion, kind, namespace, name string
}

// identity returns the document's identity.
func (d document) identity() string {
	return strings.Join([]string{d.apiVersion, d.kind, d.namespace, d.name}, "/")
}

// Validate runs Validation in its documented order. Every stage is cheap and
// read-only; a stage that refuses ends Validation before the next runs.
func (v *Validator) Validate(ctx context.Context, req Request) (Result, error) {
	result := Result{Passed: true}
	if len(req.Files) == 0 {
		result.refuse("files", "a FilePatch Proposal must change at least one file")
		return result, nil
	}
	if len(req.Files) > MaxFiles {
		result.refuse("files", "a Proposal may change at most %d files, not %d", MaxFiles, len(req.Files))
		return result, nil
	}
	if req.BaseCommit == "" {
		return Result{}, errors.New("validation needs the base commit")
	}
	if req.Repository == nil {
		return Result{}, errors.New("validation needs the base repository")
	}
	state := v.checkFiles(ctx, req, &result)
	if !result.Passed {
		return result, nil
	}
	checkIdentities(req, &result, state)
	if !result.Passed {
		return result, nil
	}
	if err := v.checkSchemas(ctx, req, &result, state); err != nil {
		return Result{}, err
	}
	if !result.Passed {
		return result, nil
	}
	if err := v.checkValuesSchema(ctx, req, &result); err != nil {
		return Result{}, err
	}
	if !result.Passed {
		return result, nil
	}
	if err := checkDiffs(ctx, req, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// fileState is what the cheap checks learned about each file.
type fileState struct {
	base     map[string]document
	proposed map[string][]document
	values   map[string]bool
}

// checkFiles runs the path, size, digest, base-blob, kind, and strict-YAML
// checks for every file, cheap before expensive.
func (v *Validator) checkFiles(ctx context.Context, req Request, result *Result) fileState {
	state := fileState{
		base:     make(map[string]document, len(req.Files)),
		proposed: make(map[string][]document, len(req.Files)),
		values:   make(map[string]bool, len(req.Files)),
	}
	for index, file := range req.Files {
		field := fmt.Sprintf("files[%d]", index)
		if refusal := checkPathPolicy(file.Path); refusal != "" {
			result.refuse(field+".path", "%s", refusal)
			continue
		}
		content, present := req.Contents[file.Path]
		if !present {
			result.refuse(field+".contentDigest", "%s has no content to validate", file.Path)
			continue
		}
		if len(content) > MaxFileBytes {
			result.refuse(field+".contentDigest",
				"%s is %d bytes, above the %d-byte limit", file.Path, len(content), MaxFileBytes)
			continue
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != file.ContentDigest {
			result.refuse(field+".contentDigest", "the content of %s does not match its digest", file.Path)
			continue
		}
		blob, err := req.Repository.File(ctx, req.BaseCommit, file.Path)
		if err != nil {
			result.refuse(field+".path", "%s is not a file at the base commit", file.Path)
			continue
		}
		if blob.SHA != file.BaseBlob {
			result.refuse(field+".baseBlob",
				"%s is %s at the base commit, not %s", file.Path, blob.SHA, file.BaseBlob)
			continue
		}
		if isValuesFile(file.Path) {
			state.values[file.Path] = true
			continue
		}
		baseDocs, errs := parseDocuments(file.Path, blob.Content)
		if len(errs) > 0 {
			result.refuse(field+".path", "%s is not a Kubernetes manifest at the base commit", file.Path)
			continue
		}
		if len(baseDocs) != 1 {
			result.refuse(field+".path",
				"%s must hold exactly one Kubernetes document, not %d", file.Path, len(baseDocs))
			continue
		}
		proposedDocs, errs := parseDocuments(file.Path, content)
		if len(errs) > 0 {
			for _, refusal := range errs {
				result.refuse(field+".updatedContent", "%s", refusal.Message)
			}
			continue
		}
		for _, doc := range proposedDocs {
			if refusal := checkKindPolicy(doc.apiVersion, doc.kind); refusal != "" {
				result.refuse(field+".updatedContent", "%s", refusal)
			}
		}
		state.base[file.Path] = baseDocs[0]
		state.proposed[file.Path] = proposedDocs
	}
	return state
}

// checkIdentities preserves the document set and each document's identity, and
// records the forbidden and permitted differences.
func checkIdentities(req Request, result *Result, state fileState) {
	for _, file := range req.Files {
		if state.values[file.Path] {
			continue
		}
		baseDoc := state.base[file.Path]
		proposedDocs := state.proposed[file.Path]
		if len(proposedDocs) != 1 {
			result.refuse("files", "%s must hold exactly one Kubernetes document, not %d",
				file.Path, len(proposedDocs))
			continue
		}
		proposedDoc := proposedDocs[0]
		if baseDoc.identity() != proposedDoc.identity() {
			result.refuse("files", "%s changes the document identity from %s to %s",
				file.Path, baseDoc.identity(), proposedDoc.identity())
			continue
		}
		for _, refusal := range compareIdentity(baseDoc.value, proposedDoc.value) {
			result.refuse("files", "%s: %s", file.Path, refusal)
		}
		result.checkDifferences(file.Path, baseDoc.value, proposedDoc.value)
	}
}

// checkSchemas validates each proposed document against the API server's own
// OpenAPI schema. A schema that is unavailable degrades the Proposal instead of
// blocking it, because the repository's Checks remain authoritative.
func (v *Validator) checkSchemas(ctx context.Context, req Request, result *Result, state fileState) error {
	if v.schemas == nil {
		result.Degraded = true
		result.warn(v1alpha1.WarningDegraded, "openapi",
			"OpenAPI schema validation did not run: admission is not evaluated and the repository's Checks are authoritative")
		return nil
	}
	for _, file := range req.Files {
		if state.values[file.Path] {
			continue
		}
		doc := state.proposed[file.Path][0]
		findings, err := v.schemas.Validate(ctx, doc.apiVersion, doc.kind, doc.value)
		switch {
		case errors.Is(err, ErrSchemaUnavailable):
			result.Degraded = true
			result.warn(v1alpha1.WarningDegraded, "openapi",
				"%s %s: no OpenAPI schema is available, so schema validation degraded", doc.apiVersion, doc.kind)
		case err != nil:
			return fmt.Errorf("validate %s against OpenAPI: %w", file.Path, err)
		default:
			for _, finding := range findings {
				result.refuse("files", "%s: %s", file.Path, finding)
			}
		}
	}
	return nil
}

// checkDiffs derives each file's Diff from the base content and bounds the
// result, so a model-supplied patch cannot be inconsistent with the base.
func checkDiffs(ctx context.Context, req Request, result *Result) error {
	byPath := make(map[string]Blob, len(req.Files))
	for _, file := range req.Files {
		blob, err := req.Repository.File(ctx, req.BaseCommit, file.Path)
		if err != nil {
			return fmt.Errorf("read %s at %s: %w", file.Path, req.BaseCommit, err)
		}
		byPath[file.Path] = blob
	}
	changed := 0
	for _, file := range req.Files {
		content, present := req.Contents[file.Path]
		if !present {
			result.refuse("files", "%s has no content to validate", file.Path)
			continue
		}
		diff, err := UnifiedDiff(file.Path, byPath[file.Path].Content, content)
		if err != nil {
			result.refuse("files", "the Diff for %s cannot be derived: %s", file.Path, err)
			continue
		}
		if diff.Diff == "" {
			result.refuse("files", "%s is unchanged", file.Path)
			continue
		}
		if len(diff.Diff) > MaxDiffBytes {
			result.refuse("files", "the Diff for %s is %d bytes, above the %d-byte limit",
				file.Path, len(diff.Diff), MaxDiffBytes)
			continue
		}
		changed += diff.Added + diff.Removed
		result.Diffs = append(result.Diffs, diff)
	}
	if changed > MaxChangedLines {
		result.refuse("files", "the Diff changes %d lines, above the %d-line limit", changed, MaxChangedLines)
	}
	return nil
}

// ChangedLines returns the number of changed lines across the derived diffs.
func (r Result) ChangedLines() int {
	total := 0
	for _, diff := range r.Diffs {
		total += diff.Added + diff.Removed
	}
	return total
}

// checkValuesSchema validates a Helm values edit against the chart's own
// values.schema.json, when the chart has one.
func (v *Validator) checkValuesSchema(ctx context.Context, req Request, result *Result) error {
	if req.ValuesSchemas == nil {
		return nil
	}
	for _, file := range req.Files {
		if !isValuesFile(file.Path) {
			continue
		}
		schema, err := req.ValuesSchemas.Schema(ctx, req.BaseCommit, file.Path)
		if err != nil {
			return fmt.Errorf("read the values schema for %s: %w", file.Path, err)
		}
		if schema == nil {
			continue
		}
		content, present := req.Contents[file.Path]
		if !present {
			result.refuse("files", "%s has no content to validate", file.Path)
			continue
		}
		findings, err := validateValuesSchema(schema, content)
		if err != nil {
			result.refuse("files", "%s: %s", file.Path, err.Error())
			continue
		}
		for _, finding := range findings {
			result.refuse("files", "%s: %s", file.Path, finding)
		}
	}
	return nil
}

// isValuesFile reports whether the path is a Helm values file.
func isValuesFile(p string) bool {
	base := strings.ToLower(path.Base(p))
	return base == "values.yaml" || base == "values.yml" || strings.HasPrefix(base, "values-")
}

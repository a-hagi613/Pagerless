// Package provenance is the four-rung ladder that grounds a Page in the Target
// Repository before the model speaks (ADR-0039). It resolves the repository,
// branch, and base commit; the file that defines the failing object; the file
// that a change should go to; and a Confidence that decides whether an
// unattended draft may be opened.
//
// The rungs, in order, are:
//
//  1. An explicit pagerless.dev/* source hint, verified against the repository
//     because anything able to patch a Target can write it.
//  2. A GitOps declaration: Argo CD's tracking-id, then the
//     app.kubernetes.io/instance label verified against the named Application,
//     then Flux Kustomization labels, then Flux HelmRelease labels, then Helm
//     release metadata.
//  3. A configured repository rule.
//  4. An identity search of the repository tree, capped at 40 files and 256 KiB,
//     with GitHub code search disabled unless the caller supplies it.
//
// A Target that none of them grounds resolves to ConfidenceNone. That is a
// correct outcome: the Page ends as Diagnosed, and no Pull Request is opened.
package provenance

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/github"
)

const (
	// pathScopedFiles and pathScopedBytes bound what the ladder reads inside a
	// directory that a declaration named. They are larger than the identity
	// search bounds because the directory, not the whole tree, is the scope.
	pathScopedFiles = 200
	pathScopedBytes = 1 << 20

	// SearchFileLimit is the number of files that an identity search reads.
	SearchFileLimit = 40
	// SearchByteLimit is the number of bytes that an identity search reads.
	SearchByteLimit = 256 << 10
	// DefaultBranch is the branch used when a declaration names none.
	DefaultBranch = "main"
	// DefaultApplicationsNamespace is where Argo CD Applications live.
	DefaultApplicationsNamespace = "argocd"

	// AnnotationEditTarget is the annotation on a Target that names the file a
	// change should go to, relative to the repository root.
	AnnotationEditTarget = "pagerless.dev/edit-target"

	// TrackingIDAnnotation is Argo CD's tracking annotation.
	TrackingIDAnnotation = "argocd.argoproj.io/tracking-id"
	// InstanceLabel is the conventional Argo CD Application label.
	InstanceLabel = "app.kubernetes.io/instance"
	// FluxKustomizationNameLabel and FluxKustomizationNamespaceLabel name the Flux
	// Kustomization that applied an object.
	FluxKustomizationNameLabel      = "kustomize.toolkit.fluxcd.io/name"
	FluxKustomizationNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"
	// FluxHelmReleaseNameLabel and FluxHelmReleaseNamespaceLabel name the Flux
	// HelmRelease that manages an object.
	FluxHelmReleaseNameLabel      = "helm.toolkit.fluxcd.io/name"
	FluxHelmReleaseNamespaceLabel = "helm.toolkit.fluxcd.io/namespace"
	// HelmReleaseNameLabel and HelmReleaseNamespaceLabel are Helm's release metadata.
	HelmReleaseNameLabel      = "meta.helm.sh/release-name"
	HelmReleaseNamespaceLabel = "meta.helm.sh/release-namespace"
)

// Object is the Target as the cluster reports it.
type Object struct {
	// Reference identifies the failing object.
	Reference v1alpha1.TargetReference
	// Annotations are the object's annotations.
	Annotations map[string]string
	// Labels are the object's labels.
	Labels map[string]string
}

// Repository is the read-only view of one Target Repository at one reference.
type Repository interface {
	// Tree returns the file paths at the reference and whether the listing was
	// truncated.
	Tree(ctx context.Context, ref string) (paths []string, truncated bool, err error)
	// File returns one file's content at the reference.
	File(ctx context.Context, ref, path string) ([]byte, error)
}

// Application is an Argo CD Application as the resolver reads it.
type Application struct {
	// Namespace and Name identify the Application.
	Namespace, Name string
	// Sources are the Application's sources. A multi-source Application has more
	// than one.
	Sources []ApplicationSource
}

// ApplicationSource is one Argo CD source. Exactly one of Path and Chart is
// normally set; a Helm Application sets Chart and may set ValueFiles.
type ApplicationSource struct {
	RepoURL        string
	TargetRevision string
	Path           string
	Chart          string
	ValueFiles     []string
}

// PatchTarget selects the objects a patch applies to.
type PatchTarget struct {
	Group, Version, Kind, Name, Namespace string
}

// FluxPatch is one patch that a Flux Kustomization applies.
type FluxPatch struct {
	Path   string
	Target *PatchTarget
}

// Kustomization is a Flux Kustomization as the resolver reads it.
type Kustomization struct {
	Namespace, Name string
	// RepositoryURL and Branch are the Git source that the Kustomization's
	// sourceRef names, resolved by the reader.
	RepositoryURL, Branch string
	// Path is the Kustomization's path inside the repository.
	Path string
	// Patches are the patches the Kustomization applies.
	Patches []FluxPatch
}

// ValuesSource is a values file that a Helm release reads from the repository.
type ValuesSource struct {
	// Path is the values file's path in the repository.
	Path string
}

// HelmRelease is a Flux HelmRelease as the resolver reads it.
type HelmRelease struct {
	Namespace, Name string
	// RepositoryURL and Branch are the Git source of a chart that lives in the
	// repository. They are empty for a chart from a HelmRepository.
	RepositoryURL, Branch string
	// ChartPath is the chart's path inside the repository.
	ChartPath string
	// ValuesFiles are the values files that the release reads from the repository.
	ValuesFiles []ValuesSource
}

// Objects reads the GitOps declarations that the cluster holds. A reader that
// cannot answer returns (nil, nil), which the ladder records as a failed rung.
type Objects interface {
	// Application returns the named Argo CD Application, or nil.
	Application(ctx context.Context, namespace, name string) (*Application, error)
	// Kustomization returns the named Flux Kustomization, or nil.
	Kustomization(ctx context.Context, namespace, name string) (*Kustomization, error)
	// HelmRelease returns the named Flux HelmRelease, or nil.
	HelmRelease(ctx context.Context, namespace, name string) (*HelmRelease, error)
}

// CodeSearch is the optional GitHub code search that the ladder may use as a
// last resort. It is nil by default, because GitHub code search is disabled.
type CodeSearch interface {
	// Search returns paths that mention the query at the reference.
	Search(ctx context.Context, ref, query string) ([]string, error)
}

// Mapping is a configured repository rule. It grounds a Target by identity
// when no declaration does.
type Mapping struct {
	// Kind, Namespace, and Name select the Target. An empty field matches any value.
	Kind      v1alpha1.TargetKind
	Namespace string
	Name      string
	// RepositoryURL, Branch, and Path name the manifest.
	RepositoryURL string
	Branch        string
	Path          string
	// EditTarget overrides the edit target. Empty lets the ladder choose it.
	EditTarget string
	// ValuesPath names a Helm values file. It is used when Path is a chart path.
	ValuesPath string
}

// Config configures a Resolver.
type Config struct {
	// Repository is the read-only Target Repository.
	Repository Repository
	// Objects reads GitOps declarations. It is required.
	Objects Objects
	// URL is the repository's HTTPS URL. It is required.
	URL string
	// Branch is the repository branch to resolve at. It defaults to DefaultBranch.
	Branch string
	// ApplicationsNamespace is where Argo CD Applications live. It defaults to
	// DefaultApplicationsNamespace.
	ApplicationsNamespace string
	// Mappings are the configured repository rules.
	Mappings []Mapping
	// HelmValues maps a Helm release name to the values file that the Operator
	// configured for it.
	HelmValues map[string]string
	// CodeSearch is the optional GitHub code search. Nil disables it.
	CodeSearch CodeSearch
	// Now returns the current time. It defaults to time.Now.
	Now func() time.Time
}

// Resolver runs the provenance ladder.
type Resolver struct {
	cfg Config
}

// New returns a Resolver.
func New(cfg Config) (*Resolver, error) {
	if cfg.Repository == nil {
		return nil, fmt.Errorf("provenance needs a repository reader")
	}
	if cfg.Objects == nil {
		return nil, fmt.Errorf("provenance needs a GitOps declaration reader")
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("provenance needs the repository URL")
	}
	if cfg.Branch == "" {
		cfg.Branch = DefaultBranch
	}
	if cfg.ApplicationsNamespace == "" {
		cfg.ApplicationsNamespace = DefaultApplicationsNamespace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Resolver{cfg: cfg}, nil
}

// Resolve descends the ladder and returns the Resolution. Every rung's outcome
// is recorded on the result, so an Operator can see why a weaker rung answered.
// A repository read that fails returns an error, because the ladder cannot
// distinguish a wrong declaration from a transient failure; a deterministic
// miss is recorded as a failed attempt and the ladder continues.
func (r *Resolver) Resolve(ctx context.Context, object Object) (v1alpha1.Resolution, error) {
	res := v1alpha1.Resolution{
		Branch:     r.cfg.Branch,
		Source:     v1alpha1.ResolutionSourceNone,
		Confidence: v1alpha1.ConfidenceNone,
		ResolvedAt: &metav1.Time{Time: r.cfg.Now()},
	}
	ladder := &ladder{resolver: r, object: object, resolution: res}
	ladder.run(ctx)
	res = ladder.resolution
	return res, nil
}

// ladder carries the state of one descent.
type ladder struct {
	resolver   *Resolver
	object     Object
	resolution v1alpha1.Resolution
	// paths is the repository tree, read at most once.
	paths    []string
	treeRead bool
	// files caches the files the ladder reads.
	files map[string][]byte
}

// run descends the rungs in order and stops at the first that resolves.
func (l *ladder) run(ctx context.Context) {
	if l.rungHint(ctx) {
		return
	}
	if l.rungTrackingID(ctx) {
		return
	}
	if l.rungInstance(ctx) {
		return
	}
	if l.rungFluxKustomization(ctx) {
		return
	}
	if l.rungFluxHelmRelease(ctx) {
		return
	}
	if l.rungHelmMetadata(ctx) {
		return
	}
	if l.rungConfiguredMapping(ctx) {
		return
	}
	if l.rungIdentitySearch(ctx) {
		return
	}
	if l.rungCodeSearch(ctx) {
		return
	}
	l.fail(v1alpha1.ResolutionSourceNone, "no rung of the provenance ladder grounds the Target in a repository")
	l.resolution.Reason = "no rung of the provenance ladder grounds the Target in a repository"
}

// attempt records rung's outcome.
func (l *ladder) attempt(source v1alpha1.ResolutionSource, outcome v1alpha1.ResolutionAttemptOutcome, reason string) {
	l.resolution.Attempts = append(l.resolution.Attempts, v1alpha1.ResolutionAttempt{
		Source: source, Outcome: outcome, Reason: reason,
	})
}

// fail records a failed rung and leaves the ladder to continue.
func (l *ladder) fail(source v1alpha1.ResolutionSource, reason string) {
	l.attempt(source, v1alpha1.ResolutionAttemptFailed, reason)
}

// skip records a rung that does not apply.
func (l *ladder) skip(source v1alpha1.ResolutionSource, reason string) {
	l.attempt(source, v1alpha1.ResolutionAttemptSkipped, reason)
}

// resolve records a successful rung.
func (l *ladder) resolve(
	source v1alpha1.ResolutionSource, confidence v1alpha1.Confidence,
	repositoryURL, manifestPath string, edit editTarget,
) bool {
	l.attempt(source, v1alpha1.ResolutionAttemptResolved, "")
	l.resolution.URL = repositoryURL
	l.resolution.Path = manifestPath
	l.resolution.EditTarget = edit.path
	l.resolution.EditTargetKind = edit.kind
	l.resolution.Source = source
	l.resolution.Confidence = confidence
	return true
}

// ambiguous records a rung whose candidates are rank-equal.
func (l *ladder) ambiguous(
	source v1alpha1.ResolutionSource, confidence v1alpha1.Confidence,
	repositoryURL string, candidates []candidate, reason string,
) bool {
	l.attempt(source, v1alpha1.ResolutionAttemptResolved, reason)
	l.resolution.URL = repositoryURL
	l.resolution.Source = source
	l.resolution.Confidence = confidence
	l.resolution.Ambiguous = true
	l.resolution.Reason = reason
	for _, candidate := range candidates {
		l.resolution.Candidates = append(l.resolution.Candidates, v1alpha1.ResolutionCandidate{
			Path: candidate.path, EditTarget: candidate.edit.path, EditTargetKind: candidate.edit.kind,
		})
	}
	return true
}

// cleanPath normalises a repository-relative path, or returns an empty string
// when it escapes the repository root.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return ""
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

// annotation returns the annotation's value, or an empty string.
func (o Object) annotation(key string) string { return o.Annotations[key] }

// label returns the label's value, or an empty string.
func (o Object) label(key string) string { return o.Labels[key] }

// repositoryURLsAgree reports whether two repository URLs name the same
// repository, accepting the .git suffix and a trailing slash.
func repositoryURLsAgree(a, b string) bool {
	normalise := func(raw string) string {
		raw = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(raw), "/"), ".git")
		return strings.TrimPrefix(raw, "https://")
	}
	return normalise(a) != "" && normalise(a) == normalise(b)
}

// githubRepository adapts the read-only GitHub client to Repository.
type githubRepository struct{ client *github.Client }

// GitHubRepository returns a Repository that reads through the read-only
// GitHub client.
func GitHubRepository(client *github.Client) Repository { return githubRepository{client: client} }

// Tree lists the files at the reference.
func (r githubRepository) Tree(ctx context.Context, ref string) ([]string, bool, error) {
	tree, err := r.client.Tree(ctx, ref)
	if err != nil {
		return nil, false, err
	}
	paths := make([]string, 0, len(tree.Entries))
	for _, entry := range tree.Files() {
		paths = append(paths, entry.Path)
	}
	return paths, tree.Truncated, nil
}

// File reads one file at the reference.
func (r githubRepository) File(ctx context.Context, ref, path string) ([]byte, error) {
	file, err := r.client.File(ctx, ref, path)
	if err != nil {
		return nil, err
	}
	return file.Content, nil
}

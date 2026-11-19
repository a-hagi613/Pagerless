package provenance

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// editTarget is one file that a change could go to, with its kind.
type editTarget struct {
	path string
	kind v1alpha1.EditTargetKind
}

// candidate is one manifest file and its edit target.
type candidate struct {
	// path is the file that defines the failing object.
	path string
	// edit is the file that a change should go to.
	edit editTarget
}

// kustomizationFile is the part of a kustomization that the resolver reads.
type kustomizationFile struct {
	Resources             []string `yaml:"resources"`
	Patches               []patch  `yaml:"patches"`
	PatchesStrategicMerge []string `yaml:"patchesStrategicMerge"`
}

// patch is one entry of a kustomization's patches list.
type patch struct {
	Path   string       `yaml:"path"`
	Target *PatchTarget `yaml:"target"`
}

// document is the part of a Kubernetes document that identifies an object.
type document struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

// group returns the API group of the document's apiVersion.
func (d document) group() string {
	group, _, _ := strings.Cut(d.APIVersion, "/")
	if !strings.Contains(d.APIVersion, "/") {
		return ""
	}
	return group
}

// defines reports whether the document is the failing object.
func (d document) defines(ref v1alpha1.TargetReference) bool {
	if d.Kind != string(ref.Kind) || d.Metadata.Name != ref.Name {
		return false
	}
	target := ref.APIVersion
	if target == "" {
		target = "apps/v1"
		if ref.Kind == v1alpha1.TargetKindPod || ref.Kind == v1alpha1.TargetKindPersistentVolumeClaim {
			target = "v1"
		}
	}
	if d.APIVersion != target {
		return false
	}
	if ref.Kind.IsClusterScoped() {
		return d.Metadata.Namespace == ""
	}
	return d.Metadata.Namespace == ref.Namespace
}

// manifests returns the Kubernetes documents in a YAML file. A file that is not
// strict YAML, such as a chart template, has no documents.
func manifests(content []byte) []document {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	var documents []document
	for {
		var doc document
		err := decoder.Decode(&doc)
		if err != nil {
			break
		}
		if doc.APIVersion == "" || doc.Kind == "" {
			continue
		}
		documents = append(documents, doc)
	}
	return documents
}

// fileDefines reports whether any document in the file is the failing object.
func fileDefines(content []byte, ref v1alpha1.TargetReference) bool {
	for _, doc := range manifests(content) {
		if doc.defines(ref) {
			return true
		}
	}
	return false
}

// isManifestPath reports whether a path can hold a Kubernetes manifest that a
// human may edit. Chart templates, Kustomize wiring, values files, and
// repository automation are not manifests for this purpose.
func isManifestPath(p string) bool {
	lower := strings.ToLower(p)
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return false
	}
	base := path.Base(lower)
	switch base {
	case "kustomization.yaml", "kustomization.yml", "values.yaml", "values.yml", "chart.yaml", "chart.yml":
		return false
	}
	for _, directory := range []string{"templates", "charts", "crds", ".github"} {
		if lower == directory || strings.HasPrefix(lower, directory+"/") ||
			strings.Contains(lower, "/"+directory+"/") {
			return false
		}
	}
	return true
}

// overlayDir returns the directories between the repository root and a
// manifest's directory, deepest first.
func overlayDirs(manifestPath string) []string {
	dir := path.Dir(manifestPath)
	var dirs []string
	for dir != "." && dir != "/" && dir != "" {
		dirs = append(dirs, dir)
		dir = path.Dir(dir)
	}
	return dirs
}

// kustomizationAt reads the kustomization in a directory, if one exists.
func (l *ladder) kustomizationAt(ctx context.Context, dir string) (*kustomizationFile, bool, error) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		content, err := l.read(ctx, path.Join(dir, name))
		if err != nil {
			continue
		}
		var file kustomizationFile
		if err := yaml.Unmarshal(content, &file); err != nil {
			return nil, false, nil
		}
		return &file, true, nil
	}
	return nil, false, nil
}

// patchTargetMatches reports whether a patch target selects the failing object.
func patchTargetMatches(target *PatchTarget, ref v1alpha1.TargetReference) bool {
	if target == nil {
		return false
	}
	if target.Kind != "" && !strings.EqualFold(target.Kind, string(ref.Kind)) {
		return false
	}
	if target.Name != "" && target.Name != ref.Name {
		return false
	}
	if target.Namespace != "" && target.Namespace != ref.Namespace {
		return false
	}
	return true
}

// patchDefines reports whether a patch file targets the failing object, either
// because it carries a matching identity or because it changes its container
// fields without naming another object.
func patchDefines(content []byte, ref v1alpha1.TargetReference) bool {
	documents := manifests(content)
	if len(documents) == 0 {
		return false
	}
	for _, doc := range documents {
		if doc.Kind == "" || !strings.EqualFold(doc.Kind, string(ref.Kind)) {
			continue
		}
		if doc.Metadata.Name != "" && doc.Metadata.Name != ref.Name {
			continue
		}
		if doc.Metadata.Namespace != "" && doc.Metadata.Namespace != ref.Namespace && !ref.Kind.IsClusterScoped() {
			continue
		}
		return true
	}
	return false
}

// fetch reads one file, returning nil when it does not exist or is not a file.
func (l *ladder) fetch(ctx context.Context, p string) []byte {
	content, err := l.read(ctx, p)
	if err != nil {
		return nil
	}
	return content
}

// overlayCandidates returns the patch and overlay-resource candidates that the
// kustomizations above a manifest offer for the failing object.
func (l *ladder) overlayCandidates(ctx context.Context, manifestPath string) []candidate {
	var candidates []candidate
	for _, dir := range overlayDirs(manifestPath) {
		file, ok, err := l.kustomizationAt(ctx, dir)
		if err != nil || !ok {
			continue
		}
		for _, entry := range append(slices.Clone(file.Patches), strategicPatches(file)...) {
			patchPath := cleanPath(path.Join(dir, entry.Path))
			if entry.Path == "" || patchPath == "" || patchPath == manifestPath {
				continue
			}
			if patchTargetMatches(entry.Target, l.object.Reference) || patchDefines(l.fetch(ctx, patchPath), l.object.Reference) {
				candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
					path: patchPath, kind: v1alpha1.EditTargetOverlayPatch,
				}})
			}
		}
		for _, resource := range file.Resources {
			resourcePath := cleanPath(path.Join(dir, resource))
			if resourcePath == "" || resourcePath == manifestPath {
				continue
			}
			content := l.fetch(ctx, resourcePath)
			if content == nil {
				continue
			}
			if fileDefines(content, l.object.Reference) {
				candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
					path: resourcePath, kind: v1alpha1.EditTargetOverlayResource,
				}})
			}
		}
	}
	return candidates
}

// strategicPatches returns the patchesStrategicMerge entries as patches.
func strategicPatches(file *kustomizationFile) []patch {
	patches := make([]patch, 0, len(file.PatchesStrategicMerge))
	for _, p := range file.PatchesStrategicMerge {
		patches = append(patches, patch{Path: p})
	}
	return patches
}

// kustomizeManifests follows a kustomization's resources and returns the files
// that define the failing object at or below the directory.
func (l *ladder) kustomizeManifests(ctx context.Context, dir string, depth int, seen map[string]struct{}) []string {
	if depth > 8 {
		return nil
	}
	if _, visited := seen[dir]; visited {
		return nil
	}
	seen[dir] = struct{}{}
	file, ok, err := l.kustomizationAt(ctx, dir)
	if err != nil || !ok {
		return nil
	}
	var manifestsFound []string
	for _, resource := range file.Resources {
		resourcePath := cleanPath(path.Join(dir, resource))
		if resourcePath == "" {
			continue
		}
		if content := l.fetch(ctx, resourcePath); content != nil {
			if fileDefines(content, l.object.Reference) {
				manifestsFound = append(manifestsFound, resourcePath)
			}
			continue
		}
		nested := l.kustomizeManifests(ctx, resourcePath, depth+1, seen)
		if len(nested) == 0 {
			// A plain directory of manifests, such as the base that an overlay
			// references, has no kustomization of its own.
			found, err := l.searchWithin(ctx, resourcePath, pathScopedFiles, pathScopedBytes)
			if err == nil {
				nested = found
			}
		}
		manifestsFound = append(manifestsFound, nested...)
	}
	return manifestsFound
}

// candidatesFor returns every edit target that a repository root offers for the
// failing object, including the manifests that the object's declarations name.
func (l *ladder) candidatesFor(ctx context.Context, root string, extras []editTarget) ([]candidate, error) {
	manifestPaths := l.kustomizeManifests(ctx, root, 0, map[string]struct{}{})
	if len(manifestPaths) == 0 {
		found, err := l.search(ctx, root)
		if err != nil {
			return nil, err
		}
		manifestPaths = found
	}
	candidates := l.manifestCandidates(ctx, manifestPaths)
	anchor := root
	if len(manifestPaths) > 0 {
		anchor = manifestPaths[0]
	}
	for _, patch := range l.kustomizationPatchEdits(ctx, root, 0, map[string]struct{}{}) {
		candidates = append(candidates, candidate{path: anchor, edit: patch})
	}
	for _, extra := range extras {
		candidates = append(candidates, candidate{path: anchor, edit: extra})
	}
	return dedupe(candidates), nil
}

// kustomizationPatchEdits returns the patches that the kustomizations at or
// below a directory apply to the failing object. An overlay patch is the
// highest-precedence edit target, whether it sits above the base or beside it.
func (l *ladder) kustomizationPatchEdits(
	ctx context.Context, dir string, depth int, seen map[string]struct{},
) []editTarget {
	if depth > 8 {
		return nil
	}
	if _, visited := seen[dir]; visited {
		return nil
	}
	seen[dir] = struct{}{}
	file, ok, err := l.kustomizationAt(ctx, dir)
	if err != nil || !ok {
		return nil
	}
	var edits []editTarget
	for _, entry := range append(slices.Clone(file.Patches), strategicPatches(file)...) {
		patchPath := cleanPath(path.Join(dir, entry.Path))
		if entry.Path == "" || patchPath == "" {
			continue
		}
		if patchTargetMatches(entry.Target, l.object.Reference) || patchDefines(l.fetch(ctx, patchPath), l.object.Reference) {
			edits = append(edits, editTarget{path: patchPath, kind: v1alpha1.EditTargetOverlayPatch})
		}
	}
	for _, resource := range file.Resources {
		resourcePath := cleanPath(path.Join(dir, resource))
		if resourcePath == "" {
			continue
		}
		edits = append(edits, l.kustomizationPatchEdits(ctx, resourcePath, depth+1, seen)...)
	}
	return edits
}

// dedupe removes repeated edit targets, keeping the highest-ranked kind.
func dedupe(candidates []candidate) []candidate {
	byEdit := make(map[string]candidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.edit.path == "" {
			continue
		}
		current, seen := byEdit[candidate.edit.path]
		if !seen || candidate.edit.kind.Rank() > current.edit.kind.Rank() {
			byEdit[candidate.edit.path] = candidate
		}
	}
	out := make([]candidate, 0, len(byEdit))
	for _, candidate := range byEdit {
		out = append(out, candidate)
	}
	slices.SortFunc(out, func(a, b candidate) int {
		switch {
		case a.edit.kind.Rank() != b.edit.kind.Rank():
			return b.edit.kind.Rank() - a.edit.kind.Rank()
		case a.edit.path < b.edit.path:
			return -1
		case a.edit.path > b.edit.path:
			return 1
		default:
			return 0
		}
	})
	return out
}

// selectEditTarget applies the fixed precedence to the candidates. It reports
// an ambiguity when two candidates rank equally.
func selectEditTarget(candidates []candidate) (editTarget, []candidate, bool) {
	if len(candidates) == 0 {
		return editTarget{}, nil, false
	}
	top := candidates[0].edit.kind.Rank()
	var best []candidate
	for _, candidate := range candidates {
		if candidate.edit.kind.Rank() != top {
			continue
		}
		best = append(best, candidate)
	}
	if len(best) > 1 {
		return editTarget{}, best, true
	}
	return best[0].edit, nil, false
}

// search reads the manifest files under a prefix, within the identity search's
// file and byte bounds, and returns the ones that define the failing object.
func (l *ladder) search(ctx context.Context, prefix string) ([]string, error) {
	return l.searchWithin(ctx, prefix, SearchFileLimit, SearchByteLimit)
}

// searchWithin reads the manifest files under a prefix within the given bounds.
func (l *ladder) searchWithin(ctx context.Context, prefix string, files, bytes int) ([]string, error) {
	paths, err := l.tree(ctx)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimSuffix(prefix, "/")
	remaining := files
	budget := bytes
	var found []string
	for _, p := range paths {
		if prefix != "" && p != prefix && !strings.HasPrefix(p, prefix+"/") {
			continue
		}
		if !isManifestPath(p) {
			continue
		}
		if remaining <= 0 || budget <= 0 {
			break
		}
		content, err := l.read(ctx, p)
		if err != nil {
			continue
		}
		remaining--
		budget -= len(content)
		if fileDefines(content, l.object.Reference) {
			found = append(found, p)
		}
	}
	return found, nil
}

// tree returns the repository tree at the resolved branch, once.
func (l *ladder) tree(ctx context.Context) ([]string, error) {
	if l.treeRead {
		return l.paths, nil
	}
	paths, truncated, err := l.resolver.cfg.Repository.Tree(ctx, l.resolver.cfg.Branch)
	if err != nil {
		return nil, fmt.Errorf("list the repository at %s: %w", l.resolver.cfg.Branch, err)
	}
	if truncated {
		return nil, fmt.Errorf("the tree at %s is truncated", l.resolver.cfg.Branch)
	}
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	l.paths, l.treeRead = sorted, true
	return l.paths, nil
}

// read returns a file at the resolved branch, caching what the ladder reads.
func (l *ladder) read(ctx context.Context, p string) ([]byte, error) {
	cleaned := cleanPath(p)
	if cleaned == "" {
		return nil, fmt.Errorf("%q is not a repository path", p)
	}
	if content, ok := l.files[cleaned]; ok {
		return content, nil
	}
	content, err := l.resolver.cfg.Repository.File(ctx, l.resolver.cfg.Branch, cleaned)
	if err != nil {
		return nil, err
	}
	if l.files == nil {
		l.files = map[string][]byte{}
	}
	l.files[cleaned] = content
	return content, nil
}

package provenance

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// rungHint runs rung one: an explicit pagerless.dev/* hint, verified.
func (l *ladder) rungHint(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceHint
	hint := cleanPath(l.object.annotation(v1alpha1.AnnotationSourcePath))
	editHint := cleanPath(l.object.annotation(AnnotationEditTarget))
	switch {
	case hint == "" && editHint == "":
		l.skip(source, "the Target carries no pagerless.dev source hint")
		return false
	case hint == "":
		l.fail(source, "the Target carries an edit-target hint but no source-path hint")
		return false
	}
	content, err := l.read(ctx, hint)
	if err != nil {
		l.fail(source, "the source-path hint names a file that is not in the repository")
		return false
	}
	if !fileDefines(content, l.object.Reference) {
		l.fail(source, "the source-path hint names a file that does not define the Target")
		return false
	}
	candidates := []candidate{{path: hint, edit: editTarget{
		path: hint, kind: v1alpha1.EditTargetBaseManifest,
	}}}
	candidates = append(candidates, l.overlayCandidates(ctx, hint)...)
	if editHint != "" {
		if l.fetch(ctx, editHint) == nil {
			l.fail(source, "the edit-target hint names a file that is not in the repository")
			return false
		}
		// An explicit edit-target hint is the Operator's declaration, so it is
		// recorded as given rather than ranked against the files above it.
		return l.resolve(source, v1alpha1.ConfidenceExact, l.resolver.cfg.URL, hint, editTarget{
			path: editHint, kind: kindOfEditPath(editHint),
		})
	}
	return l.commit(source, v1alpha1.ConfidenceExact, hint, candidates)
}

// rungTrackingID runs Argo CD's tracking-id rung.
func (l *ladder) rungTrackingID(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceArgoCDTrackingID
	raw := l.object.annotation(TrackingIDAnnotation)
	if raw == "" {
		l.skip(source, "the Target has no Argo CD tracking-id")
		return false
	}
	application, identity, ok := parseTrackingID(raw)
	if !ok {
		l.fail(source, "the Argo CD tracking-id is not in app:group/Kind:namespace/name form")
		return false
	}
	if !identity.matches(l.object.Reference) {
		l.fail(source, "the Argo CD tracking-id names another object")
		return false
	}
	app, err := l.resolver.cfg.Objects.Application(ctx, l.resolver.cfg.ApplicationsNamespace, application)
	if err != nil {
		l.fail(source, fmt.Sprintf("read the Argo CD Application %s: %v", application, err))
		return false
	}
	if app == nil {
		l.fail(source, fmt.Sprintf("the Argo CD Application %s is not readable", application))
		return false
	}
	candidates, reason := l.applicationCandidates(ctx, app)
	if len(candidates) == 0 {
		l.fail(source, reason)
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceExact, "", candidates)
}

// rungInstance runs the app.kubernetes.io/instance rung. The label is accepted
// as exact only when the named Application's repository and path also agree;
// otherwise the resolution is an identity.
func (l *ladder) rungInstance(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceArgoCDInstance
	instance := l.object.label(InstanceLabel)
	if instance == "" {
		l.skip(source, "the Target has no app.kubernetes.io/instance label")
		return false
	}
	app, err := l.resolver.cfg.Objects.Application(ctx, l.resolver.cfg.ApplicationsNamespace, instance)
	if err != nil {
		l.fail(source, fmt.Sprintf("read the Argo CD Application %s: %v", instance, err))
		return false
	}
	if app == nil {
		l.fail(source, fmt.Sprintf("no Argo CD Application is named %s", instance))
		return false
	}
	if len(app.Sources) == 1 && repositoryURLsAgree(app.Sources[0].RepoURL, l.resolver.cfg.URL) {
		if candidates, _ := l.applicationCandidates(ctx, app); len(candidates) > 0 {
			return l.commit(source, v1alpha1.ConfidenceExact, "", candidates)
		}
	}
	found, err := l.search(ctx, "")
	if err != nil {
		l.fail(source, err.Error())
		return false
	}
	if len(found) == 0 {
		l.fail(source, fmt.Sprintf("the Application %s does not agree with the repository and no file defines the Target", instance))
		return false
	}
	candidates := l.manifestCandidates(ctx, found)
	return l.commit(source, v1alpha1.ConfidenceIdentity, "", candidates)
}

// rungFluxKustomization runs Flux's Kustomization rung.
func (l *ladder) rungFluxKustomization(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceFluxKustomization
	name := l.object.label(FluxKustomizationNameLabel)
	if name == "" {
		l.skip(source, "the Target has no Flux Kustomization label")
		return false
	}
	namespace := l.object.label(FluxKustomizationNamespaceLabel)
	if namespace == "" {
		l.fail(source, "the Flux Kustomization label has no namespace label beside it")
		return false
	}
	kustomization, err := l.resolver.cfg.Objects.Kustomization(ctx, namespace, name)
	if err != nil {
		l.fail(source, fmt.Sprintf("read the Flux Kustomization %s/%s: %v", namespace, name, err))
		return false
	}
	if kustomization == nil {
		l.fail(source, fmt.Sprintf("the Flux Kustomization %s/%s is not readable", namespace, name))
		return false
	}
	if !repositoryURLsAgree(kustomization.RepositoryURL, l.resolver.cfg.URL) {
		l.fail(source, "the Flux Kustomization deploys another repository")
		return false
	}
	var extras []editTarget
	for _, entry := range kustomization.Patches {
		if entry.Path == "" || !patchTargetMatches(entry.Target, l.object.Reference) {
			continue
		}
		patchPath := cleanPath(path.Join(kustomization.Path, entry.Path))
		if patchPath != "" {
			extras = append(extras, editTarget{path: patchPath, kind: v1alpha1.EditTargetFluxPatch})
		}
	}
	candidates, err := l.candidatesFor(ctx, kustomization.Path, extras)
	if err != nil {
		l.fail(source, err.Error())
		return false
	}
	if len(candidates) == 0 {
		l.fail(source, fmt.Sprintf("the Flux Kustomization path %s holds no file that defines the Target", kustomization.Path))
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceConfigured, "", candidates)
}

// rungFluxHelmRelease runs Flux's HelmRelease rung.
func (l *ladder) rungFluxHelmRelease(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceFluxHelmRelease
	name := l.object.label(FluxHelmReleaseNameLabel)
	if name == "" {
		l.skip(source, "the Target has no Flux HelmRelease label")
		return false
	}
	namespace := l.object.label(FluxHelmReleaseNamespaceLabel)
	if namespace == "" {
		l.fail(source, "the Flux HelmRelease label has no namespace label beside it")
		return false
	}
	release, err := l.resolver.cfg.Objects.HelmRelease(ctx, namespace, name)
	if err != nil {
		l.fail(source, fmt.Sprintf("read the Flux HelmRelease %s/%s: %v", namespace, name, err))
		return false
	}
	if release == nil {
		l.fail(source, fmt.Sprintf("the Flux HelmRelease %s/%s is not readable", namespace, name))
		return false
	}
	return l.helmRelease(ctx, source, release)
}

// rungHelmMetadata runs Helm's release metadata rung.
func (l *ladder) rungHelmMetadata(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceHelmMetadata
	name := l.object.label(HelmReleaseNameLabel)
	if name == "" {
		l.skip(source, "the Target has no Helm release metadata")
		return false
	}
	namespace := l.object.label(HelmReleaseNamespaceLabel)
	if release, err := l.resolver.cfg.Objects.HelmRelease(ctx, namespace, name); err == nil && release != nil {
		if l.helmRelease(ctx, source, release) {
			return true
		}
	}
	if app, err := l.resolver.cfg.Objects.Application(ctx, l.resolver.cfg.ApplicationsNamespace, name); err == nil && app != nil {
		if candidates, _ := l.applicationCandidates(ctx, app); len(candidates) > 0 {
			return l.commit(source, v1alpha1.ConfidenceConfigured, "", candidates)
		}
	}
	if values := cleanPath(l.resolver.cfg.HelmValues[name]); values != "" {
		if l.fetch(ctx, values) != nil {
			return l.commit(source, v1alpha1.ConfidenceConfigured, values, []candidate{{
				path: values, edit: editTarget{path: values, kind: v1alpha1.EditTargetHelmValues},
			}})
		}
	}
	found, err := l.searchKind(ctx, v1alpha1.TargetKind("HelmRelease"), name, namespace)
	if err != nil {
		l.fail(source, err.Error())
		return false
	}
	if len(found) == 0 {
		l.fail(source, fmt.Sprintf("the Helm release %s/%s has no file in the repository", namespace, name))
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceNameOnly, "", l.manifestCandidates(ctx, found))
}

// rungConfiguredMapping runs the configured repository rules.
func (l *ladder) rungConfiguredMapping(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceConfiguredMapping
	var matched []Mapping
	for _, mapping := range l.resolver.cfg.Mappings {
		if mapping.Kind != "" && mapping.Kind != l.object.Reference.Kind {
			continue
		}
		if mapping.Namespace != "" && mapping.Namespace != l.object.Reference.Namespace {
			continue
		}
		if mapping.Name != "" && mapping.Name != l.object.Reference.Name {
			continue
		}
		matched = append(matched, mapping)
	}
	if len(matched) == 0 {
		l.skip(source, "no configured repository rule selects the Target")
		return false
	}
	var candidates []candidate
	var reasons []string
	for _, mapping := range matched {
		if !repositoryURLsAgree(mapping.RepositoryURL, l.resolver.cfg.URL) {
			reasons = append(reasons, "a configured rule names another repository")
			continue
		}
		manifestPath := cleanPath(mapping.Path)
		content := l.fetch(ctx, manifestPath)
		if content == nil || !fileDefines(content, l.object.Reference) {
			reasons = append(reasons, fmt.Sprintf("the configured path %s does not define the Target", manifestPath))
			continue
		}
		candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
			path: manifestPath, kind: v1alpha1.EditTargetBaseManifest,
		}})
		candidates = append(candidates, l.overlayCandidates(ctx, manifestPath)...)
		if mapping.EditTarget != "" {
			candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
				path: cleanPath(mapping.EditTarget), kind: kindOfEditPath(mapping.EditTarget),
			}})
		}
		if mapping.ValuesPath != "" {
			candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
				path: cleanPath(mapping.ValuesPath), kind: v1alpha1.EditTargetHelmValues,
			}})
		}
	}
	if len(candidates) == 0 {
		l.fail(source, strings.Join(reasons, "; "))
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceConfigured, "", dedupe(candidates))
}

// rungIdentitySearch searches the repository tree for the failing object.
func (l *ladder) rungIdentitySearch(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceIdentitySearch
	found, err := l.search(ctx, "")
	if err != nil {
		l.fail(source, err.Error())
		return false
	}
	if len(found) == 0 {
		l.fail(source, fmt.Sprintf("no file in the first %d manifest files defines the Target", SearchFileLimit))
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceIdentity, "", l.manifestCandidates(ctx, found))
}

// rungCodeSearch runs the optional GitHub code search as a last resort.
func (l *ladder) rungCodeSearch(ctx context.Context) bool {
	source := v1alpha1.ResolutionSourceIdentitySearch
	search := l.resolver.cfg.CodeSearch
	if search == nil {
		l.skip(source, "GitHub code search is disabled")
		return false
	}
	paths, err := search.Search(ctx, l.resolver.cfg.Branch,
		fmt.Sprintf("%s %s", l.object.Reference.Kind, l.object.Reference.Name))
	if err != nil {
		l.fail(source, fmt.Sprintf("GitHub code search failed: %v", err))
		return false
	}
	var found []string
	for _, p := range paths {
		cleaned := cleanPath(p)
		if cleaned == "" || !isManifestPath(cleaned) || len(found) == SearchFileLimit {
			continue
		}
		if content := l.fetch(ctx, cleaned); content != nil && fileDefines(content, l.object.Reference) {
			found = append(found, cleaned)
		}
	}
	if len(found) == 0 {
		l.fail(source, "GitHub code search returned no file that defines the Target")
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceIdentity, "", l.manifestCandidates(ctx, found))
}

// commit selects the edit target and records the resolution.
func (l *ladder) commit(
	source v1alpha1.ResolutionSource, confidence v1alpha1.Confidence,
	manifestPath string, candidates []candidate,
) bool {
	edit, best, ambiguous := selectEditTarget(dedupe(candidates))
	if ambiguous {
		reason := "two or more files are equally valid edit targets"
		if len(best) > 0 && best[0].path != "" {
			reason = fmt.Sprintf("%d files are equally valid edit targets for %s", len(best), best[0].path)
		}
		l.ambiguous(source, confidence, l.resolver.cfg.URL, best, reason)
		return true
	}
	if edit.path == "" {
		l.fail(source, "no file is a valid edit target")
		return false
	}
	if manifestPath == "" {
		manifestPath = manifestAnchor(best, candidates)
	}
	l.resolve(source, confidence, l.resolver.cfg.URL, manifestPath, edit)
	return true
}

// manifestAnchor returns the file that defines the object: the first candidate
// whose path is not its own edit target, or the first candidate's path.
func manifestAnchor(best, candidates []candidate) string {
	for _, candidate := range append(slices.Clone(best), candidates...) {
		if candidate.path != candidate.edit.path {
			return candidate.path
		}
	}
	if len(candidates) > 0 {
		return candidates[0].path
	}
	return ""
}

// manifestCandidates returns the edit targets for a set of manifest files,
// including the overlays above them.
func (l *ladder) manifestCandidates(ctx context.Context, manifests []string) []candidate {
	candidates := make([]candidate, 0, len(manifests))
	for _, manifestPath := range manifests {
		candidates = append(candidates, candidate{path: manifestPath, edit: editTarget{
			path: manifestPath, kind: v1alpha1.EditTargetBaseManifest,
		}})
		candidates = append(candidates, l.overlayCandidates(ctx, manifestPath)...)
	}
	return candidates
}

// applicationCandidates returns the edit targets that an Argo CD Application's
// sources offer. Two sources that both ground the object are an ambiguity, not
// a guess.
func (l *ladder) applicationCandidates(ctx context.Context, app *Application) ([]candidate, string) {
	var candidates []candidate
	var reasons []string
	for _, source := range app.Sources {
		if !repositoryURLsAgree(source.RepoURL, l.resolver.cfg.URL) {
			reasons = append(reasons, fmt.Sprintf("the Application source %s is another repository", source.RepoURL))
			continue
		}
		if source.Chart != "" {
			values := l.helmValueCandidates(ctx, source)
			if len(values) == 0 {
				reasons = append(reasons, "the Application renders a chart with no values file")
				continue
			}
			candidates = append(candidates, values...)
			continue
		}
		found, err := l.candidatesFor(ctx, source.Path, nil)
		if err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		if len(found) == 0 {
			reasons = append(reasons, fmt.Sprintf("the Application path %s holds no file that defines the Target", source.Path))
			continue
		}
		candidates = append(candidates, found...)
	}
	if len(candidates) == 0 && len(reasons) == 0 {
		reasons = append(reasons, "the Application has no source")
	}
	return dedupe(candidates), strings.Join(reasons, "; ")
}

// helmValueCandidates returns the values files of an Argo CD Helm source.
func (l *ladder) helmValueCandidates(ctx context.Context, source ApplicationSource) []candidate {
	var candidates []candidate
	for _, values := range source.ValueFiles {
		if strings.HasPrefix(values, "$") {
			// $values/... references another source, which is not an edit target here.
			continue
		}
		valuesPath := cleanPath(path.Join(source.Path, values))
		if valuesPath == "" {
			continue
		}
		if l.fetch(ctx, valuesPath) == nil {
			continue
		}
		candidates = append(candidates, candidate{
			path: valuesPath, edit: editTarget{path: valuesPath, kind: v1alpha1.EditTargetArgoValues},
		})
	}
	return candidates
}

// helmRelease resolves a Flux HelmRelease or a Helm release to its files.
func (l *ladder) helmRelease(ctx context.Context, source v1alpha1.ResolutionSource, release *HelmRelease) bool {
	var candidates []candidate
	manifestPath := ""
	if release.RepositoryURL != "" && repositoryURLsAgree(release.RepositoryURL, l.resolver.cfg.URL) {
		for _, values := range release.ValuesFiles {
			valuesPath := cleanPath(path.Join(release.ChartPath, values.Path))
			if valuesPath == "" || l.fetch(ctx, valuesPath) == nil {
				continue
			}
			if manifestPath == "" {
				manifestPath = valuesPath
			}
			candidates = append(candidates, candidate{
				path: valuesPath, edit: editTarget{path: valuesPath, kind: v1alpha1.EditTargetHelmValues},
			})
		}
		if len(candidates) > 0 {
			return l.commit(source, v1alpha1.ConfidenceConfigured, manifestPath, candidates)
		}
	}
	found, err := l.searchKind(ctx, "HelmRelease", release.Name, release.Namespace)
	if err != nil {
		l.fail(source, err.Error())
		return false
	}
	if len(found) == 0 {
		l.fail(source, fmt.Sprintf("the HelmRelease %s/%s has no file in the repository", release.Namespace, release.Name))
		return false
	}
	return l.commit(source, v1alpha1.ConfidenceConfigured, "", l.manifestCandidates(ctx, found))
}

// searchKind searches the manifest files for a document of the kind, name, and
// namespace.
func (l *ladder) searchKind(ctx context.Context, kind v1alpha1.TargetKind, name, namespace string) ([]string, error) {
	paths, err := l.tree(ctx)
	if err != nil {
		return nil, err
	}
	remaining := SearchFileLimit
	budget := SearchByteLimit
	var found []string
	for _, p := range paths {
		if !isManifestPath(p) || remaining <= 0 || budget <= 0 {
			continue
		}
		content, err := l.read(ctx, p)
		if err != nil {
			continue
		}
		remaining--
		budget -= len(content)
		for _, doc := range manifests(content) {
			if doc.Kind != string(kind) || doc.Metadata.Name != name {
				continue
			}
			if namespace != "" && doc.Metadata.Namespace != namespace {
				continue
			}
			found = append(found, p)
			break
		}
	}
	return found, nil
}

// kindOfEditPath returns the edit-target kind that a path's form implies.
func kindOfEditPath(p string) v1alpha1.EditTargetKind {
	base := strings.ToLower(path.Base(p))
	switch {
	case base == "values.yaml" || base == "values.yml" || strings.Contains(base, "values."):
		return v1alpha1.EditTargetHelmValues
	case strings.HasSuffix(base, ".json"):
		return v1alpha1.EditTargetArgoValues
	default:
		return v1alpha1.EditTargetBaseManifest
	}
}

// trackedObject is the identity that Argo CD's tracking-id carries. It has no
// version, so a match compares the group, kind, namespace, and name.
type trackedObject struct {
	Group, Kind, Namespace, Name string
}

// matches reports whether the tracked identity is the failing object.
func (t trackedObject) matches(ref v1alpha1.TargetReference) bool {
	if !strings.EqualFold(t.Kind, string(ref.Kind)) || t.Name != ref.Name {
		return false
	}
	if ref.Kind.IsClusterScoped() {
		return t.Namespace == ""
	}
	return t.Namespace == ref.Namespace && t.Group == groupOfAPIVersion(ref.APIVersion)
}

// groupOfAPIVersion returns an apiVersion's group, or the conventional group of
// a Target whose apiVersion is unset.
func groupOfAPIVersion(apiVersion string) string {
	if apiVersion == "" {
		return ""
	}
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

// parseTrackingID parses Argo CD's tracking-id, whose form is
// app:group/Kind:namespace/name. Defensive parsing is deliberate: the
// annotation is written by Argo CD but read from an object that anything able
// to patch a Target can touch.
func parseTrackingID(raw string) (string, trackedObject, bool) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", trackedObject{}, false
	}
	group, kind, ok := strings.Cut(parts[1], "/")
	if !ok || kind == "" {
		return "", trackedObject{}, false
	}
	var namespace, name string
	if index := strings.LastIndex(parts[2], "/"); index >= 0 {
		namespace, name = parts[2][:index], parts[2][index+1:]
	} else {
		name = parts[2]
	}
	if name == "" {
		return "", trackedObject{}, false
	}
	return parts[0], trackedObject{Group: group, Kind: kind, Namespace: namespace, Name: name}, true
}

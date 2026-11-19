package provenance_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/provenance"
)

const (
	repositoryURL     = "https://github.com/acme/gitops"
	defaultBranch     = "main"
	targetName        = "checkout"
	targetNS          = "shop"
	deploymentFile    = "apps/checkout/deployment.yaml"
	manifestsCheckout = "manifests/shop/checkout.yaml"
	manifestsPatch    = "manifests/shop/patch.yaml"
	prodValues        = "environments/prod/checkout-values.yaml"
)

// fakeRepo is a Target Repository held in memory.
type fakeRepo struct {
	files     map[string]string
	truncated bool
	reads     int
}

func newRepo(files map[string]string) *fakeRepo { return &fakeRepo{files: files} }

func (r *fakeRepo) Tree(context.Context, string) ([]string, bool, error) {
	return slices.Sorted(maps.Keys(r.files)), r.truncated, nil
}

func (r *fakeRepo) File(_ context.Context, _, path string) ([]byte, error) {
	content, ok := r.files[path]
	if !ok {
		return nil, errors.New("not found")
	}
	r.reads++
	return []byte(content), nil
}

// fakeObjects is the GitOps declarations the cluster holds.
type fakeObjects struct {
	applications   map[string]*provenance.Application
	kustomizations map[string]*provenance.Kustomization
	releases       map[string]*provenance.HelmRelease
}

func (o *fakeObjects) Application(_ context.Context, _, name string) (*provenance.Application, error) {
	return o.applications[name], nil
}

func (o *fakeObjects) Kustomization(_ context.Context, namespace, name string) (*provenance.Kustomization, error) {
	return o.kustomizations[namespace+"/"+name], nil
}

func (o *fakeObjects) HelmRelease(_ context.Context, namespace, name string) (*provenance.HelmRelease, error) {
	return o.releases[namespace+"/"+name], nil
}

// deployment is a Deployment manifest of the fixture namespace.
func deployment(name string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 2
`, name, targetNS)
}

// resolver returns a Resolver over the fixtures.
func resolver(t *testing.T, repo provenance.Repository, objects provenance.Objects) *provenance.Resolver {
	t.Helper()
	r, err := provenance.New(provenance.Config{
		Repository: repo,
		Objects:    objects,
		URL:        repositoryURL,
		Branch:     defaultBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// checkout is the failing Deployment that most fixtures use.
func checkout() provenance.Object {
	return provenance.Object{Reference: v1alpha1.TargetReference{
		APIVersion: "apps/v1", Kind: v1alpha1.TargetKindDeployment, Namespace: targetNS, Name: targetName,
	}}
}

// withAnnotations returns the object with the annotations added.
func withAnnotations(object provenance.Object, annotations map[string]string) provenance.Object {
	object.Annotations = annotations
	return object
}

// withLabels returns the object with the labels added.
func withLabels(object provenance.Object, labels map[string]string) provenance.Object {
	object.Labels = labels
	return object
}

// resolve runs the ladder and fails the test on an error.
func resolve(t *testing.T, r *provenance.Resolver, object provenance.Object) v1alpha1.Resolution {
	t.Helper()
	resolution, err := r.Resolve(context.Background(), object)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return resolution
}

func TestArgoCDTrackingIDResolvesTheOverlayPatch(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		"apps/checkout/base/deployment.yaml": deployment(targetName),
		"apps/checkout/overlays/prod/kustomization.yaml": `resources:
  - ../../base
patches:
  - path: replicas.yaml
    target:
      group: apps
      version: v1
      kind: Deployment
      name: checkout
`,
		"apps/checkout/overlays/prod/replicas.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
spec:
  replicas: 4
`,
	})
	objects := &fakeObjects{applications: map[string]*provenance.Application{
		targetName: {
			Namespace: "argocd", Name: targetName,
			Sources: []provenance.ApplicationSource{{
				RepoURL: repositoryURL, Path: "apps/checkout/overlays/prod", TargetRevision: defaultBranch,
			}},
		},
	}}
	object := withAnnotations(checkout(), map[string]string{
		provenance.TrackingIDAnnotation: "checkout:apps/Deployment:shop/checkout",
	})
	got := resolve(t, resolver(t, repo, objects), object)
	if got.Source != v1alpha1.ResolutionSourceArgoCDTrackingID || got.Confidence != v1alpha1.ConfidenceExact {
		t.Fatalf("source = %s, confidence = %s, want ArgoCDTrackingID/exact", got.Source, got.Confidence)
	}
	if got.Path != "apps/checkout/base/deployment.yaml" {
		t.Errorf("path = %q, want the base manifest", got.Path)
	}
	if got.EditTarget != "apps/checkout/overlays/prod/replicas.yaml" ||
		got.EditTargetKind != v1alpha1.EditTargetOverlayPatch {
		t.Errorf("editTarget = %q (%s), want the overlay patch", got.EditTarget, got.EditTargetKind)
	}
	if got.Ambiguous {
		t.Error("the resolution is ambiguous, want one overlay patch")
	}
	if got.URL != repositoryURL || got.Branch != defaultBranch {
		t.Errorf("repository = %s/%s, want %s/main", got.URL, got.Branch, repositoryURL)
	}
}

func TestArgoCDMultiSourceApplicationIsAmbiguous(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		deploymentFile:                   deployment(targetName),
		"apps/checkout2/deployment.yaml": deployment(targetName),
	})
	objects := &fakeObjects{applications: map[string]*provenance.Application{
		targetName: {
			Namespace: "argocd", Name: targetName,
			Sources: []provenance.ApplicationSource{
				{RepoURL: repositoryURL, Path: "apps/checkout"},
				{RepoURL: repositoryURL, Path: "apps/checkout2"},
			},
		},
	}}
	object := withAnnotations(checkout(), map[string]string{
		provenance.TrackingIDAnnotation: "checkout:apps/Deployment:shop/checkout",
	})
	got := resolve(t, resolver(t, repo, objects), object)
	if !got.Ambiguous {
		t.Fatalf("ambiguous = false, want true: %+v", got)
	}
	if got.Source != v1alpha1.ResolutionSourceArgoCDTrackingID {
		t.Errorf("source = %s, want the tracking-id rung", got.Source)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(got.Candidates), got.Candidates)
	}
	if got.Reason == "" {
		t.Error("an ambiguous resolution must carry a reason")
	}
}

func TestVerifiedHintWins(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		manifestsCheckout: deployment(targetName),
		manifestsPatch: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: shop
spec:
  replicas: 3
`,
	})
	object := withAnnotations(checkout(), map[string]string{
		v1alpha1.AnnotationSourcePath:   manifestsCheckout,
		provenance.AnnotationEditTarget: manifestsPatch,
	})
	got := resolve(t, resolver(t, repo, &fakeObjects{}), object)
	if got.Source != v1alpha1.ResolutionSourceHint || got.Confidence != v1alpha1.ConfidenceExact {
		t.Fatalf("source = %s, confidence = %s, want Hint/exact", got.Source, got.Confidence)
	}
	if got.Path != manifestsCheckout {
		t.Errorf("path = %q, want the hinted manifest", got.Path)
	}
	if got.EditTarget != manifestsPatch || got.EditTargetKind != v1alpha1.EditTargetBaseManifest {
		t.Errorf("editTarget = %q (%s), want the hinted patch", got.EditTarget, got.EditTargetKind)
	}
}

func TestWrongHintIsIgnoredAndTheLadderContinues(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		deploymentFile: deployment(targetName),
	})
	repo.files["apps/other/deployment.yaml"] = deployment("payments")
	object := withAnnotations(checkout(), map[string]string{
		v1alpha1.AnnotationSourcePath: "apps/other/deployment.yaml",
	})
	got := resolve(t, resolver(t, repo, &fakeObjects{}), object)
	if got.Source != v1alpha1.ResolutionSourceIdentitySearch || got.Confidence != v1alpha1.ConfidenceIdentity {
		t.Fatalf("source = %s, confidence = %s, want IdentitySearch/identity", got.Source, got.Confidence)
	}
	if got.Path != deploymentFile {
		t.Errorf("path = %q, want the file that defines the Deployment", got.Path)
	}
	var hint v1alpha1.ResolutionAttempt
	for _, attempt := range got.Attempts {
		if attempt.Source == v1alpha1.ResolutionSourceHint {
			hint = attempt
		}
	}
	if hint.Outcome != v1alpha1.ResolutionAttemptFailed || !strings.Contains(hint.Reason, "does not define") {
		t.Errorf("hint attempt = %+v, want a failure that says the file does not define the Target", hint)
	}
}

func TestFluxKustomizationPatchIsTheEditTarget(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		"clusters/prod/shop/deployment.yaml": deployment(targetName),
		"clusters/prod/shop/replicas.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
spec:
  replicas: 5
`,
	})
	objects := &fakeObjects{kustomizations: map[string]*provenance.Kustomization{
		"flux-system/shop-flux": {
			Namespace: "flux-system", Name: "shop-flux",
			RepositoryURL: repositoryURL, Branch: defaultBranch, Path: "clusters/prod/shop",
			Patches: []provenance.FluxPatch{{
				Path: "replicas.yaml",
				Target: &provenance.PatchTarget{
					Group: "apps", Version: "v1", Kind: "Deployment", Name: targetName,
				},
			}},
		},
	}}
	object := withLabels(checkout(), map[string]string{
		provenance.FluxKustomizationNameLabel:      "shop-flux",
		provenance.FluxKustomizationNamespaceLabel: "flux-system",
	})
	got := resolve(t, resolver(t, repo, objects), object)
	if got.Source != v1alpha1.ResolutionSourceFluxKustomization ||
		got.Confidence != v1alpha1.ConfidenceConfigured {
		t.Fatalf("source = %s, confidence = %s, want FluxKustomization/configured", got.Source, got.Confidence)
	}
	if got.Path != "clusters/prod/shop/deployment.yaml" {
		t.Errorf("path = %q, want the base manifest", got.Path)
	}
	if got.EditTarget != "clusters/prod/shop/replicas.yaml" ||
		got.EditTargetKind != v1alpha1.EditTargetFluxPatch {
		t.Errorf("editTarget = %q (%s), want the Flux patch", got.EditTarget, got.EditTargetKind)
	}
}

func TestFluxHelmReleaseResolvesItsValuesFile(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		"charts/checkout/Chart.yaml":       "apiVersion: v2\nname: checkout\nversion: 1.0.0\n",
		"charts/checkout/values.yaml":      "replicas: 2\n",
		"charts/checkout/values-prod.yaml": "replicas: 3\n",
		"charts/checkout/templates/dep.yaml": "apiVersion: apps/v1\nkind: Deployment\n" +
			"metadata:\n  name: {{ .Release.Name }}\n",
	})
	objects := &fakeObjects{releases: map[string]*provenance.HelmRelease{
		"shop/checkout": {
			Namespace: targetNS, Name: targetName,
			RepositoryURL: repositoryURL, Branch: defaultBranch, ChartPath: "charts/checkout",
			ValuesFiles: []provenance.ValuesSource{{Path: "values-prod.yaml"}},
		},
	}}
	object := withLabels(checkout(), map[string]string{
		provenance.FluxHelmReleaseNameLabel:      targetName,
		provenance.FluxHelmReleaseNamespaceLabel: targetNS,
	})
	got := resolve(t, resolver(t, repo, objects), object)
	if got.Source != v1alpha1.ResolutionSourceFluxHelmRelease ||
		got.Confidence != v1alpha1.ConfidenceConfigured {
		t.Fatalf("source = %s, confidence = %s, want FluxHelmRelease/configured", got.Source, got.Confidence)
	}
	if got.EditTarget != "charts/checkout/values-prod.yaml" ||
		got.EditTargetKind != v1alpha1.EditTargetHelmValues {
		t.Errorf("editTarget = %q (%s), want the values file", got.EditTarget, got.EditTargetKind)
	}
}

func TestHelmMetadataUsesTheConfiguredValuesFile(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		prodValues: "replicas: 3\n",
	})
	resolver, err := provenance.New(provenance.Config{
		Repository: repo,
		Objects:    &fakeObjects{},
		URL:        repositoryURL,
		HelmValues: map[string]string{targetName: prodValues},
	})
	if err != nil {
		t.Fatal(err)
	}
	object := withLabels(checkout(), map[string]string{
		provenance.HelmReleaseNameLabel:      targetName,
		provenance.HelmReleaseNamespaceLabel: targetNS,
	})
	got := resolve(t, resolver, object)
	if got.Source != v1alpha1.ResolutionSourceHelmMetadata ||
		got.Confidence != v1alpha1.ConfidenceConfigured {
		t.Fatalf("source = %s, confidence = %s, want HelmMetadata/configured", got.Source, got.Confidence)
	}
	if got.EditTarget != prodValues ||
		got.EditTargetKind != v1alpha1.EditTargetHelmValues {
		t.Errorf("editTarget = %q (%s), want the configured values file", got.EditTarget, got.EditTargetKind)
	}
}

func TestConfiguredMappingResolvesTheManifest(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		manifestsCheckout: deployment(targetName),
	})
	resolver, err := provenance.New(provenance.Config{
		Repository: repo,
		Objects:    &fakeObjects{},
		URL:        repositoryURL,
		Mappings: []provenance.Mapping{{
			Kind: v1alpha1.TargetKindDeployment, Namespace: targetNS, Name: targetName,
			RepositoryURL: repositoryURL, Branch: defaultBranch, Path: manifestsCheckout,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := resolve(t, resolver, checkout())
	if got.Source != v1alpha1.ResolutionSourceConfiguredMapping ||
		got.Confidence != v1alpha1.ConfidenceConfigured {
		t.Fatalf("source = %s, confidence = %s, want ConfiguredMapping/configured", got.Source, got.Confidence)
	}
	if got.Path != manifestsCheckout || got.EditTarget != manifestsCheckout {
		t.Errorf("path/editTarget = %q/%q, want the mapped manifest", got.Path, got.EditTarget)
	}
}

func TestIdentitySearchStopsAtTheFileLimit(t *testing.T) {
	t.Parallel()
	files := map[string]string{}
	for index := range 40 {
		files[fmt.Sprintf("manifests/%02d.yaml", index)] = deployment(fmt.Sprintf("other-%d", index))
	}
	files["manifests/zz-last.yaml"] = deployment(targetName)
	got := resolve(t, resolver(t, newRepo(files), &fakeObjects{}), checkout())
	if got.Confidence != v1alpha1.ConfidenceNone {
		t.Fatalf("confidence = %s, want none: the search must stop after %d files",
			got.Confidence, provenance.SearchFileLimit)
	}
	if got.Reason == "" {
		t.Error("an unresolved resolution must carry a reason")
	}
	if len(got.Attempts) == 0 {
		t.Error("an unresolved resolution must record why each rung failed")
	}
	if len(got.Attempts) > 10 {
		t.Errorf("%d attempts exceed the recorded bound of 10", len(got.Attempts))
	}
}

func TestAmbiguousIdentitySearchReturnsCandidates(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{
		"manifests/a/checkout.yaml": deployment(targetName),
		"manifests/b/checkout.yaml": deployment(targetName),
	})
	got := resolve(t, resolver(t, repo, &fakeObjects{}), checkout())
	if !got.Ambiguous || len(got.Candidates) != 2 {
		t.Fatalf("resolution = %+v, want two ambiguous candidates", got)
	}
	if got.Confidence != v1alpha1.ConfidenceIdentity {
		t.Errorf("confidence = %s, want identity", got.Confidence)
	}
}

func TestSelfManagedNodeResolvesToNone(t *testing.T) {
	t.Parallel()
	object := provenance.Object{Reference: v1alpha1.TargetReference{Kind: v1alpha1.TargetKindNode, Name: "worker-1"}}
	got := resolve(t, resolver(t, newRepo(map[string]string{}), &fakeObjects{}), object)
	if got.Source != v1alpha1.ResolutionSourceNone || got.Confidence != v1alpha1.ConfidenceNone {
		t.Fatalf("source = %s, confidence = %s, want None/none", got.Source, got.Confidence)
	}
	if got.URL != "" || got.EditTarget != "" {
		t.Errorf("a none resolution must name no repository: %+v", got)
	}
}

func TestWrongTrackingIDIsIgnored(t *testing.T) {
	t.Parallel()
	repo := newRepo(map[string]string{deploymentFile: deployment(targetName)})
	object := withAnnotations(checkout(), map[string]string{
		provenance.TrackingIDAnnotation: "checkout:apps/Deployment:shop/payments",
	})
	got := resolve(t, resolver(t, repo, &fakeObjects{}), object)
	if got.Source != v1alpha1.ResolutionSourceIdentitySearch {
		t.Fatalf("source = %s, want the ladder to continue to IdentitySearch", got.Source)
	}
}

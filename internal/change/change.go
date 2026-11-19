// Package change performs the Validation that the executable Actions share
// (ADR-0002, ADR-0008): the namespace allowlist, reading the live Workload,
// locating its manifest in the Sandbox, rendering the exact change, and
// deciding when a Proposal has become a Stale Proposal. PatchResources and
// Scale supply the rendering and bounds of their own change.
package change

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// maxPatchBytes is the largest patch that a Proposal can store.
const maxPatchBytes = 32768

// fieldWorkload is the Validation field of the Workload itself.
const fieldWorkload = "workload"

// ErrNoLiveWorkload means that the Workload does not exist.
var ErrNoLiveWorkload = errors.New("the Workload does not exist")

// Config configures a Validator.
type Config struct {
	// Reader reads Workloads directly from the API server, not from a cache.
	Reader client.Reader
	// Sandbox is the repository that holds the Workloads' manifests.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Workloads may be changed. None are allowed when it is empty.
	AllowedNamespaces []string
}

// LiveWorkload is a Deployment or StatefulSet read from the cluster.
type LiveWorkload struct {
	// Object is the Deployment or StatefulSet.
	Object client.Object
	// PodSpec is the pod template's spec.
	PodSpec *corev1.PodSpec
}

// Replicas returns the Workload's desired replica count.
func (w *LiveWorkload) Replicas() int32 {
	switch object := w.Object.(type) {
	case *appsv1.Deployment:
		if object.Spec.Replicas != nil {
			return *object.Spec.Replicas
		}
	case *appsv1.StatefulSet:
		if object.Spec.Replicas != nil {
			return *object.Spec.Replicas
		}
	}
	return 1
}

// SetReplicas sets the Workload's desired replica count on the live object, for
// a server-side dry run.
func (w *LiveWorkload) SetReplicas(replicas int32) {
	switch object := w.Object.(type) {
	case *appsv1.Deployment:
		object.Spec.Replicas = &replicas
	case *appsv1.StatefulSet:
		object.Spec.Replicas = &replicas
	}
}

// Renderer renders one pull request Action's change and dry-runs it. A
// Validator runs the steps that every such Action shares; the Renderer supplies
// only what is specific to its Action.
type Renderer interface {
	// Action is the Action whose change the Renderer renders.
	Action() v1alpha1.Action
	// ParametersField is the Validation field of the Action's parameters.
	ParametersField() string
	// CheckParameters returns the Validation errors in the requested parameters.
	CheckParameters() []agentapi.ValidationError
	// Check enforces the Action's rules on the live Workload, before the Sandbox is read.
	Check(live *LiveWorkload) []agentapi.ValidationError
	// Render returns the manifest with the change, and any Validation errors in
	// the result. The live Workload and the source path are passed so that a
	// renderer can enforce rules that depend on them. A non-nil error means that
	// the manifest cannot be edited safely.
	Render(
		ctx context.Context, src []byte, workload v1alpha1.WorkloadReference,
		live *LiveWorkload, sourcePath string,
	) ([]byte, []agentapi.ValidationError, error)
	// DryRun applies the change to the live Workload with a server-side dry run.
	DryRun(ctx context.Context, live *LiveWorkload) ([]agentapi.ValidationError, error)
}

// Validator performs the shared Validation and staleness checks.
type Validator struct {
	cfg Config
}

// New returns a Validator.
func New(cfg Config) *Validator { return &Validator{cfg: cfg} }

// NamespaceErrors returns the Validation errors when the Workload's namespace
// is not one that the Action may change.
func (v *Validator) NamespaceErrors(
	ref v1alpha1.WorkloadReference, action v1alpha1.Action,
) []agentapi.ValidationError {
	if slices.Contains(v.cfg.AllowedNamespaces, ref.Namespace) {
		return nil
	}
	return []agentapi.ValidationError{{
		Field:   "workload.namespace",
		Message: fmt.Sprintf("namespace %s is not in the allowlist for %s", ref.Namespace, action),
	}}
}

// Live returns the Workload as the API server holds it.
func (v *Validator) Live(
	ctx context.Context, ref v1alpha1.WorkloadReference,
) (*LiveWorkload, error) {
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	var live LiveWorkload
	switch ref.Kind {
	case v1alpha1.WorkloadKindDeployment:
		var d appsv1.Deployment
		live = LiveWorkload{Object: &d, PodSpec: &d.Spec.Template.Spec}
	case v1alpha1.WorkloadKindStatefulSet:
		var s appsv1.StatefulSet
		live = LiveWorkload{Object: &s, PodSpec: &s.Spec.Template.Spec}
	default:
		return nil, fmt.Errorf("unsupported Workload kind %q", ref.Kind)
	}
	if err := v.cfg.Reader.Get(ctx, key, live.Object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNoLiveWorkload
		}
		return nil, fmt.Errorf("read %s %s: %w", ref.Kind, key, err)
	}
	return &live, nil
}

// Validate runs the Validation that every pull request Action shares: the
// namespace allowlist, the Action's parameter bounds, the live Workload and its
// manifest, the rendered patch, and a server-side dry run. When it passes, the
// returned Outcome holds the exact patch and the base it was rendered against.
func (v *Validator) Validate(
	ctx context.Context, req proposal.Request, r Renderer,
) (proposal.Outcome, error) {
	workload := req.Triage.Spec.Workload
	errs := v.NamespaceErrors(workload, r.Action())
	errs = append(errs, r.CheckParameters()...)
	if len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}

	live, err := v.Live(ctx, workload)
	if errors.Is(err, ErrNoLiveWorkload) {
		return refuse(fieldWorkload,
			fmt.Sprintf("%s %s/%s does not exist", workload.Kind, workload.Namespace, workload.Name)), nil
	}
	if err != nil {
		return proposal.Outcome{}, err
	}
	sourcePath := live.Object.GetAnnotations()[v1alpha1.AnnotationSourcePath]
	if sourcePath == "" {
		return refuse(fieldWorkload, fmt.Sprintf("the Workload has no %s annotation, so its manifest cannot be located",
			v1alpha1.AnnotationSourcePath)), nil
	}
	if err := sandbox.CheckPath(sourcePath); err != nil {
		return refuse(fieldWorkload, fmt.Sprintf("annotation %s: %v", v1alpha1.AnnotationSourcePath, err)), nil
	}
	if errs := r.Check(live); len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}

	checkout, err := v.cfg.Sandbox.Checkout(ctx)
	if err != nil {
		return proposal.Outcome{}, err
	}
	defer func() { _ = checkout.Close() }()
	original, err := checkout.ReadFile(ctx, sourcePath)
	switch {
	case errors.Is(err, sandbox.ErrNotFound), errors.Is(err, sandbox.ErrInvalidPath):
		return refuse(fieldWorkload, fmt.Sprintf("annotation %s: %v", v1alpha1.AnnotationSourcePath, err)), nil
	case err != nil:
		return proposal.Outcome{}, err
	}
	rendered, errs, err := r.Render(ctx, original, workload, live, sourcePath)
	if len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}
	if err != nil {
		return refuse(fieldWorkload, fmt.Sprintf("the manifest %s cannot be rendered: %v", sourcePath, err)), nil
	}
	patch, err := checkout.Diff(ctx, sourcePath, rendered)
	if err != nil {
		return proposal.Outcome{}, err
	}
	switch {
	case patch == "":
		return refuse(r.ParametersField(),
			fmt.Sprintf("the requested %s change is already what the Sandbox declares", r.Action())), nil
	case len(patch) > maxPatchBytes:
		return refuse(r.ParametersField(), fmt.Sprintf("the rendered patch exceeds %d bytes", maxPatchBytes)), nil
	}

	// The dry run overwrites the object with its result, whose generation is the one the change would produce.
	generation := live.Object.GetGeneration()
	if errs, err := r.DryRun(ctx, live); err != nil || len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, err
	}
	return proposal.Outcome{
		Change: &v1alpha1.RenderedChange{Path: sourcePath, Patch: patch},
		Base:   &v1alpha1.ProposalBase{Commit: checkout.Commit, WorkloadGeneration: generation},
	}, nil
}

// Stale reports why the Proposal's base no longer holds, or an empty string
// when it still does. The base holds while the Workload's generation and, for a
// pull request Action, its source path and the Sandbox file the patch changes
// are unchanged.
func (v *Validator) Stale(ctx context.Context, t *v1alpha1.Triage) (string, error) {
	p := t.Status.Proposal
	if p == nil || p.Base == nil {
		return "", nil
	}
	live, err := v.Live(ctx, t.Spec.Workload)
	if errors.Is(err, ErrNoLiveWorkload) {
		return v1alpha1.ReasonWorkloadDeleted, nil
	}
	if err != nil {
		return "", err
	}
	change := p.RenderedChange
	switch {
	// The source path is checked first, because changing an annotation also bumps a Deployment's generation.
	case change != nil && live.Object.GetAnnotations()[v1alpha1.AnnotationSourcePath] != change.Path:
		return v1alpha1.ReasonSourcePathChanged, nil
	case live.Object.GetGeneration() != p.Base.WorkloadGeneration:
		return v1alpha1.ReasonWorkloadGenerationChanged, nil
	case change == nil:
		// A live Action has nothing to render against beyond the generation.
		return "", nil
	}
	head, err := v.cfg.Sandbox.Head(ctx)
	if err != nil {
		return "", err
	}
	if head == p.Base.Commit {
		return "", nil
	}
	checkout, err := v.cfg.Sandbox.Checkout(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = checkout.Close() }()
	blob, _, err := checkout.Blob(ctx, change.Path)
	if errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, sandbox.ErrInvalidPath) {
		return v1alpha1.ReasonSandboxFileChanged, nil
	}
	if err != nil {
		return "", err
	}
	if patchedBlob := baseBlob(change.Patch); patchedBlob == "" || patchedBlob != blob {
		return v1alpha1.ReasonSandboxFileChanged, nil
	}
	return "", nil
}

// baseBlob returns the identifier of the file that a patch from sandbox.Diff
// changes, as recorded on the patch's index line.
func baseBlob(patch string) string {
	for line := range strings.SplitSeq(patch, "\n") {
		if rest, ok := strings.CutPrefix(line, "index "); ok {
			before, _, _ := strings.Cut(rest, "..")
			return before
		}
	}
	return ""
}

func refuse(field, message string) proposal.Outcome {
	return proposal.Outcome{Errors: []agentapi.ValidationError{{Field: field, Message: message}}}
}

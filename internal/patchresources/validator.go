// Package patchresources performs Validation for the PatchResources Action:
// the namespace allowlist, the bounds on requests and limits, a server-side
// dry run of the patched Workload, and the rendering of the exact git patch
// against the Sandbox (ADR-0002, ADR-0008). It also decides when a rendered
// Proposal has become a Stale Proposal.
package patchresources

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// maxPatchBytes is the largest patch that a Proposal can store.
const maxPatchBytes = 32768

// maxMessageLength bounds a message that is copied from another system into a Validation error.
const maxMessageLength = 1000

const (
	fieldPrefix   = "proposal.parameters.patchResources"
	fieldWorkload = "workload"
)

var errNoLiveWorkload = errors.New("the Workload does not exist")

// Bounds is the range that one resource's requests and limits must stay within, inclusive.
type Bounds struct {
	// Min is the smallest permitted value.
	Min resource.Quantity
	// Max is the largest permitted value.
	Max resource.Quantity
}

// DefaultBounds returns the bounds that apply unless configured otherwise.
func DefaultBounds() map[corev1.ResourceName]Bounds {
	return map[corev1.ResourceName]Bounds{
		corev1.ResourceCPU:    {Min: resource.MustParse("10m"), Max: resource.MustParse("4")},
		corev1.ResourceMemory: {Min: resource.MustParse("16Mi"), Max: resource.MustParse("8Gi")},
	}
}

// Config configures the PatchResources validator.
type Config struct {
	// Reader reads Workloads directly from the API server, not from a cache.
	Reader client.Reader
	// Writer sends the dry-run update. Nothing is ever persisted by it.
	Writer client.Writer
	// Sandbox is the repository that holds the Workloads' manifests.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Workloads may be patched. None are allowed when it is empty.
	AllowedNamespaces []string
	// Bounds are the permitted range for each resource a Proposal may set. A resource without bounds is refused.
	Bounds map[corev1.ResourceName]Bounds
}

// Validator validates PatchResources Proposals and renders their patch.
type Validator struct {
	cfg Config
}

// New returns a Validator.
func New(cfg Config) *Validator { return &Validator{cfg: cfg} }

var _ proposal.ActionValidator = (*Validator)(nil)
var _ proposal.StalenessChecker = (*Validator)(nil)

var containerNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks the Proposal and, when it passes, renders the patch.
func (v *Validator) Validate(ctx context.Context, req proposal.Request) (proposal.Outcome, error) {
	var params v1alpha1.PatchResourcesParameters
	if p := req.Action.Parameters; p != nil && p.PatchResources != nil {
		params = *p.PatchResources
	} else {
		return refuse(fieldPrefix, "patchResources parameters are required"), nil
	}
	workload := req.Triage.Spec.Workload

	var errs []agentapi.ValidationError
	if !slices.Contains(v.cfg.AllowedNamespaces, workload.Namespace) {
		errs = append(errs, agentapi.ValidationError{
			Field:   "workload.namespace",
			Message: fmt.Sprintf("namespace %s is not in the allowlist for PatchResources", workload.Namespace),
		})
	}
	errs = append(errs, v.checkParameters(params)...)
	if len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}

	live, err := v.workload(ctx, workload)
	if errors.Is(err, errNoLiveWorkload) {
		return refuse(fieldWorkload,
			fmt.Sprintf("%s %s/%s does not exist", workload.Kind, workload.Namespace, workload.Name)), nil
	}
	if err != nil {
		return proposal.Outcome{}, err
	}
	sourcePath := live.object.GetAnnotations()[v1alpha1.AnnotationSourcePath]
	if sourcePath == "" {
		return refuse(fieldWorkload, fmt.Sprintf("the Workload has no %s annotation, so its manifest cannot be located",
			v1alpha1.AnnotationSourcePath)), nil
	}
	if err := sandbox.CheckPath(sourcePath); err != nil {
		return refuse(fieldWorkload, fmt.Sprintf("annotation %s: %v", v1alpha1.AnnotationSourcePath, err)), nil
	}
	if findLiveContainer(live.podSpec, params.Container) == nil {
		return refuse(fieldPrefix+".container",
			fmt.Sprintf("the Workload has no container named %s", params.Container)), nil
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
	rendered, merged, err := render(original, workload, params.Container, params.Requests, params.Limits)
	if err != nil {
		return refuse(fieldWorkload, fmt.Sprintf("the manifest %s cannot be patched: %v", sourcePath, err)), nil
	}
	if errs := checkMerged(merged); len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, nil
	}
	if errs, err := v.checkUnschedulable(
		ctx, live, workload, sourcePath, params.Container, merged); err != nil || len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, err
	}
	patch, err := checkout.Diff(ctx, sourcePath, rendered)
	if err != nil {
		return proposal.Outcome{}, err
	}
	switch {
	case patch == "":
		return refuse(fieldPrefix, "the requested resources are already what the Sandbox declares"), nil
	case len(patch) > maxPatchBytes:
		return refuse(fieldPrefix, fmt.Sprintf("the rendered patch exceeds %d bytes", maxPatchBytes)), nil
	}

	// The dry run overwrites the object with its result, whose generation is the one the change would produce.
	generation := live.object.GetGeneration()
	if errs, err := v.dryRun(ctx, live, params); err != nil || len(errs) > 0 {
		return proposal.Outcome{Errors: errs}, err
	}
	return proposal.Outcome{
		Change: &v1alpha1.RenderedChange{Path: sourcePath, Patch: patch},
		Base:   &v1alpha1.ProposalBase{Commit: checkout.Commit, WorkloadGeneration: generation},
	}, nil
}

func refuse(field, message string) proposal.Outcome {
	return proposal.Outcome{Errors: []agentapi.ValidationError{{Field: field, Message: message}}}
}

// checkParameters enforces the configured bounds on the requested values.
func (v *Validator) checkParameters(p v1alpha1.PatchResourcesParameters) []agentapi.ValidationError {
	var errs []agentapi.ValidationError
	if len(p.Container) > 63 || !containerNamePattern.MatchString(p.Container) {
		errs = append(errs, agentapi.ValidationError{
			Field: fieldPrefix + ".container", Message: "container must be a valid container name",
		})
	}
	if len(p.Requests) == 0 && len(p.Limits) == 0 {
		errs = append(errs, agentapi.ValidationError{Field: fieldPrefix, Message: "set at least one request or limit"})
	}
	for _, section := range []struct {
		field string
		list  corev1.ResourceList
	}{{"requests", p.Requests}, {"limits", p.Limits}} {
		for _, name := range sortedNames(section.list) {
			field := fmt.Sprintf("%s.%s[%s]", fieldPrefix, section.field, name)
			bounds, ok := v.cfg.Bounds[name]
			if !ok {
				errs = append(errs, agentapi.ValidationError{Field: field,
					Message: fmt.Sprintf("resource %s is not allowed; allowed: %v", name, sortedNames(boundsList(v.cfg.Bounds)))})
				continue
			}
			quantity := section.list[name]
			if quantity.Cmp(bounds.Min) < 0 || quantity.Cmp(bounds.Max) > 0 {
				errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
					"%s is outside the permitted range %s to %s", quantity.String(), bounds.Min.String(), bounds.Max.String())})
			}
		}
	}
	return errs
}

// checkMerged enforces that no request exceeds its limit in the resources the Sandbox will hold.
func checkMerged(r corev1.ResourceRequirements) []agentapi.ValidationError {
	var errs []agentapi.ValidationError
	for _, name := range sortedNames(r.Requests) {
		request := r.Requests[name]
		if limit, ok := r.Limits[name]; ok && request.Cmp(limit) > 0 {
			errs = append(errs, agentapi.ValidationError{
				Field:   fmt.Sprintf("%s.requests[%s]", fieldPrefix, name),
				Message: fmt.Sprintf("the %s request %s would exceed its limit %s", name, request.String(), limit.String()),
			})
		}
	}
	return errs
}

// errNoKnownGood means that the bound of an unschedulable Workload cannot be
// checked, because the Sandbox's known-good state does not declare the value.
var errNoKnownGood = errors.New("the Unschedulable bound cannot be checked against the Sandbox's known-good state")

// checkUnschedulable enforces the bound that applies when the live Workload's
// requests cannot fit any node: the Proposal must set each such request within
// the largest value that any node reports as allocatable, and no higher than
// the value that the Sandbox's known-good state declares.
func (v *Validator) checkUnschedulable(
	ctx context.Context, live *liveWorkload, workload v1alpha1.WorkloadReference, sourcePath, container string,
	merged corev1.ResourceRequirements,
) ([]agentapi.ValidationError, error) {
	var nodes corev1.NodeList
	if err := v.cfg.Reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list the nodes for the Unschedulable bound: %w", err)
	}
	largest := largestAllocatable(nodes.Items)
	if len(largest) == 0 {
		return nil, nil
	}
	current := findLiveContainer(live.podSpec, container).Resources.Requests
	var constrained []corev1.ResourceName
	for _, name := range sortedNames(current) {
		request := current[name]
		if allocatable, ok := largest[name]; !ok || request.Cmp(allocatable) > 0 {
			constrained = append(constrained, name)
		}
	}
	if len(constrained) == 0 {
		return nil, nil
	}
	knownGood, err := v.knownGoodRequests(ctx, workload, sourcePath, container)
	switch {
	case errors.Is(err, errNoKnownGood):
		return []agentapi.ValidationError{{Field: fieldPrefix, Message: err.Error()}}, nil
	case err != nil:
		return nil, err
	}
	var errs []agentapi.ValidationError
	for _, name := range constrained {
		field := fmt.Sprintf("%s.requests[%s]", fieldPrefix, name)
		bound, declared := knownGood[name]
		reach, reachable := largest[name]
		requested := merged.Requests[name]
		switch {
		case !declared:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"%v: it declares no %s request for container %s", errNoKnownGood, name, container)})
		case !reachable:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"no node reports allocatable %s, so a %s request cannot fit one", name, name)})
		case reach.Cmp(requested) < 0:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"the Workload is unschedulable because its %s request %s fits no node; "+
					"the request must not exceed the largest node's allocatable %s",
				name, requested.String(), reach.String())})
		case bound.Cmp(requested) < 0:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"the %s request %s exceeds the known-good request %s",
				name, requested.String(), bound.String())})
		}
	}
	return errs, nil
}

// largestAllocatable returns, for each resource, the largest allocatable value
// that any node reports. A resource that no node reports is absent.
func largestAllocatable(nodes []corev1.Node) corev1.ResourceList {
	largest := corev1.ResourceList{}
	for i := range nodes {
		for name, quantity := range nodes[i].Status.Allocatable {
			if current, ok := largest[name]; !ok || quantity.Cmp(current) > 0 {
				largest[name] = quantity
			}
		}
	}
	return largest
}

// knownGoodRequests returns the requests that the Sandbox's known-good state
// declares for the container. It reports errNoKnownGood when that state does
// not declare them, because the bound must not be guessed.
func (v *Validator) knownGoodRequests(
	ctx context.Context, workload v1alpha1.WorkloadReference, sourcePath, container string,
) (corev1.ResourceList, error) {
	history, err := v.cfg.Sandbox.History(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = history.Close() }()
	content, err := history.FileAtTag(ctx, sandbox.KnownGoodTag, sourcePath)
	if errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, sandbox.ErrInvalidPath) {
		return nil, fmt.Errorf("%w: tag %s holds no %s", errNoKnownGood, sandbox.KnownGoodTag, sourcePath)
	}
	if err != nil {
		return nil, err
	}
	docs, err := parseDocuments(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	index, err := findWorkload(docs, workload)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	containerNode, err := findContainer(docs[index], container)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	_, resourcesValue := mapEntry(containerNode, "resources")
	var knownGood corev1.ResourceRequirements
	if resourcesValue != nil {
		if knownGood, err = decodeResources(resourcesValue); err != nil {
			return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
		}
	}
	return knownGood.Requests, nil
}

func sortedNames(list corev1.ResourceList) []corev1.ResourceName {
	names := make([]corev1.ResourceName, 0, len(list))
	for name := range list {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func boundsList(b map[corev1.ResourceName]Bounds) corev1.ResourceList {
	list := make(corev1.ResourceList, len(b))
	for name := range b {
		list[name] = resource.Quantity{}
	}
	return list
}

// liveWorkload is a Deployment or StatefulSet read from the cluster.
type liveWorkload struct {
	object  client.Object
	podSpec *corev1.PodSpec
}

func (v *Validator) workload(ctx context.Context, ref v1alpha1.WorkloadReference) (*liveWorkload, error) {
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	var live liveWorkload
	switch ref.Kind {
	case v1alpha1.WorkloadKindDeployment:
		var d appsv1.Deployment
		live = liveWorkload{object: &d, podSpec: &d.Spec.Template.Spec}
	case v1alpha1.WorkloadKindStatefulSet:
		var s appsv1.StatefulSet
		live = liveWorkload{object: &s, podSpec: &s.Spec.Template.Spec}
	default:
		return nil, fmt.Errorf("unsupported Workload kind %q", ref.Kind)
	}
	if err := v.cfg.Reader.Get(ctx, key, live.object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNoLiveWorkload
		}
		return nil, fmt.Errorf("read %s %s: %w", ref.Kind, key, err)
	}
	return &live, nil
}

func findLiveContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

// dryRun applies the change to the live Workload with a server-side dry run,
// which runs the API server's own validation and admission. A refusal by the
// API server is a Validation error; any other failure means Validation could not run.
func (v *Validator) dryRun(ctx context.Context, live *liveWorkload, p v1alpha1.PatchResourcesParameters,
) ([]agentapi.ValidationError, error) {
	container := findLiveContainer(live.podSpec, p.Container)
	container.Resources = overlay(container.Resources, p.Requests, p.Limits)
	// The dry run asks whether the patched object is acceptable, not whether nothing changed since it was read;
	// a later change makes the Proposal stale, which is checked separately.
	live.object.SetResourceVersion("")
	err := v.cfg.Writer.Update(ctx, live.object, client.DryRunAll)
	switch {
	case err == nil:
		return nil, nil
	case apierrors.IsInvalid(err), apierrors.IsForbidden(err), apierrors.IsBadRequest(err):
		return []agentapi.ValidationError{{
			Field:   fieldWorkload,
			Message: truncate("the server-side dry run was refused: "+err.Error(), maxMessageLength),
		}}, nil
	default:
		return nil, fmt.Errorf("dry-run the patched Workload: %w", err)
	}
}

func truncate(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit]))
	}
	return s
}

// Stale reports why the Proposal's base no longer holds, or an empty string
// when it still does. The base holds while the Workload's generation and its
// source path are unchanged and the Sandbox file that the patch changes is
// the one the patch was rendered against.
func (v *Validator) Stale(ctx context.Context, t *v1alpha1.Triage) (string, error) {
	p := t.Status.Proposal
	if p == nil || p.Base == nil || p.RenderedChange == nil {
		return "", nil
	}
	live, err := v.workload(ctx, t.Spec.Workload)
	if errors.Is(err, errNoLiveWorkload) {
		return v1alpha1.ReasonWorkloadDeleted, nil
	}
	if err != nil {
		return "", err
	}
	switch {
	// The source path is checked first, because changing an annotation also bumps a Deployment's generation.
	case live.object.GetAnnotations()[v1alpha1.AnnotationSourcePath] != p.RenderedChange.Path:
		return v1alpha1.ReasonSourcePathChanged, nil
	case live.object.GetGeneration() != p.Base.WorkloadGeneration:
		return v1alpha1.ReasonWorkloadGenerationChanged, nil
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
	blob, _, err := checkout.Blob(ctx, p.RenderedChange.Path)
	if errors.Is(err, sandbox.ErrNotFound) || errors.Is(err, sandbox.ErrInvalidPath) {
		return v1alpha1.ReasonSandboxFileChanged, nil
	}
	if err != nil {
		return "", err
	}
	if patchedBlob := baseBlob(p.RenderedChange.Patch); patchedBlob == "" || patchedBlob != blob {
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

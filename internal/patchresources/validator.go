// Package patchresources performs Validation for the PatchResources Action:
// the namespace allowlist, the bounds on requests and limits, a server-side
// dry run of the patched Target, and the rendering of the exact git patch
// against the Sandbox (ADR-0002, ADR-0008). It also decides when a rendered
// Proposal has become a Stale Proposal.
package patchresources

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/change"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/sandboxyaml"
)

// maxMessageLength bounds a message that is copied from another system into a Validation error.

const fieldPrefix = "proposal.parameters.patchResources"

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
	// Reader reads Targets directly from the API server, not from a cache.
	Reader client.Reader
	// Sandbox is the repository that holds the Targets' manifests.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Targets may be patched. None are allowed when it is empty.
	AllowedNamespaces []string
	// Bounds are the permitted range for each resource a Proposal may set. A resource without bounds is refused.
	Bounds map[corev1.ResourceName]Bounds
}

// Validator validates PatchResources Proposals and renders their patch.
type Validator struct {
	change *change.Validator
	cfg    Config
}

// New returns a Validator.
func New(cfg Config) *Validator {
	return &Validator{
		change: change.New(change.Config{
			Reader:            cfg.Reader,
			Sandbox:           cfg.Sandbox,
			AllowedNamespaces: cfg.AllowedNamespaces,
		}),
		cfg: cfg,
	}
}

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
	return v.change.Validate(ctx, req, &renderer{
		params:  params,
		bounds:  v.cfg.Bounds,
		reader:  v.cfg.Reader,
		sandbox: v.cfg.Sandbox,
	})
}

// Stale reports why the Proposal's base no longer holds, or an empty string
// when it still does.
func (v *Validator) Stale(ctx context.Context, t *v1alpha1.Page) (string, error) {
	return v.change.Stale(ctx, t)
}

// renderer renders the resources change of one Proposal and dry-runs it.
type renderer struct {
	params  v1alpha1.PatchResourcesParameters
	bounds  map[corev1.ResourceName]Bounds
	reader  client.Reader
	sandbox sandbox.Repository
}

var _ change.Renderer = (*renderer)(nil)

func (r *renderer) Action() v1alpha1.Action { return v1alpha1.ActionPatchResources }

func (r *renderer) ParametersField() string { return fieldPrefix }

func (r *renderer) CheckParameters() []agentapi.ValidationError {
	return checkParameters(r.params, r.bounds)
}

func (r *renderer) Check(live *change.LiveTarget) []agentapi.ValidationError {
	if findLiveContainer(live.PodSpec, r.params.Container) != nil {
		return nil
	}
	return []agentapi.ValidationError{{
		Field:   fieldPrefix + ".container",
		Message: fmt.Sprintf("the Target has no container named %s", r.params.Container),
	}}
}

func (r *renderer) Render(
	ctx context.Context, src []byte, target v1alpha1.TargetReference,
	live *change.LiveTarget, sourcePath string,
) ([]byte, []agentapi.ValidationError, error) {
	rendered, merged, err := render(src, target, r.params.Container, r.params.Requests, r.params.Limits)
	if err != nil {
		return nil, nil, err
	}
	if errs := checkMerged(merged); len(errs) > 0 {
		return nil, errs, nil
	}
	// The live Target can differ from the Sandbox copy while a GitOps controller
	// has not applied a change yet, so the resources the API server will see are
	// checked as well. It is an in-process check: the product holds no update
	// grant, so there is no server-side dry run (ADR-0037).
	if container := findLiveContainer(live.PodSpec, r.params.Container); container != nil {
		if errs := checkMerged(overlay(container.Resources, r.params.Requests, r.params.Limits)); len(errs) > 0 {
			return nil, errs, nil
		}
	}
	if errs, err := r.checkUnschedulable(
		ctx, live, target, sourcePath, r.params.Container, merged); err != nil || len(errs) > 0 {
		return nil, errs, err
	}
	return rendered, nil, nil
}

func refuse(field, message string) proposal.Outcome {
	return proposal.Outcome{Errors: []agentapi.ValidationError{{Field: field, Message: message}}}
}

// checkParameters enforces the configured bounds on the requested values.
func checkParameters(
	p v1alpha1.PatchResourcesParameters, permitted map[corev1.ResourceName]Bounds,
) []agentapi.ValidationError {
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
			limit, ok := permitted[name]
			if !ok {
				errs = append(errs, agentapi.ValidationError{Field: field,
					Message: fmt.Sprintf("resource %s is not allowed; allowed: %v", name, sortedNames(boundsList(permitted)))},
				)
				continue
			}
			quantity := section.list[name]
			if quantity.Cmp(limit.Min) < 0 || quantity.Cmp(limit.Max) > 0 {
				errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
					"%s is outside the permitted range %s to %s", quantity.String(), limit.Min.String(), limit.Max.String())})
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

// errNoKnownGood means that the Sandbox's known-good tag is absent or does not
// declare the container's resources, so only node allocatable bounds apply.
var errNoKnownGood = errors.New("the Unschedulable bound cannot be checked against the Sandbox's known-good state")

// checkUnschedulable enforces the bound that applies when the live Target's
// requests cannot fit any node: the Proposal must set each such request within
// the largest value that any node reports as allocatable, and, when the
// Sandbox's known-good tag declares a request for that resource, no higher
// than that known-good value.
func (r *renderer) checkUnschedulable(
	ctx context.Context, live *change.LiveTarget, target v1alpha1.TargetReference,
	sourcePath, container string, merged corev1.ResourceRequirements,
) ([]agentapi.ValidationError, error) {
	var nodes corev1.NodeList
	if err := r.reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list the nodes for the Unschedulable bound: %w", err)
	}
	largest := largestAllocatable(nodes.Items)
	if len(largest) == 0 {
		return nil, nil
	}
	current := findLiveContainer(live.PodSpec, container).Resources.Requests
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
	knownGood, err := r.knownGoodRequests(ctx, target, sourcePath, container)
	if err != nil && !errors.Is(err, errNoKnownGood) {
		return nil, err
	}
	var errs []agentapi.ValidationError
	for _, name := range constrained {
		field := fmt.Sprintf("%s.requests[%s]", fieldPrefix, name)
		bound, declared := knownGood[name]
		reach, reachable := largest[name]
		requested := merged.Requests[name]
		switch {
		case !reachable:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"no node reports allocatable %s, so a %s request cannot fit one", name, name)})
		case reach.Cmp(requested) < 0:
			errs = append(errs, agentapi.ValidationError{Field: field, Message: fmt.Sprintf(
				"the Target is unschedulable because its %s request %s fits no node; "+
					"the request must not exceed the largest node's allocatable %s",
				name, requested.String(), reach.String())})
		case declared && bound.Cmp(requested) < 0:
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
// declares for the container. It reports errNoKnownGood when that tag or
// container declaration is absent.
func (r *renderer) knownGoodRequests(
	ctx context.Context, target v1alpha1.TargetReference, sourcePath, container string,
) (corev1.ResourceList, error) {
	history, err := r.sandbox.History(ctx)
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
	docs, err := sandboxyaml.ParseDocuments(content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	index, err := sandboxyaml.FindTarget(docs, target)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	containerNode, err := findContainer(docs[index], container)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at tag %s: %v", errNoKnownGood, sourcePath, sandbox.KnownGoodTag, err)
	}
	_, resourcesValue := sandboxyaml.Entry(containerNode, "resources")
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

func findLiveContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

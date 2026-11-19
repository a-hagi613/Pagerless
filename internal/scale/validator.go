// Package scale performs Validation for the Scale Action: the namespace
// allowlist, the configured replica bounds, a server-side dry run of the scaled
// Target, and the rendering of the exact replica-count patch against the
// Sandbox (ADR-0002, ADR-0008). It also decides when a rendered Proposal has
// become a Stale Proposal.
package scale

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/change"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/sandbox"
)

// maxMessageLength bounds a message that is copied from another system into a Validation error.

const fieldPrefix = "proposal.parameters.scale"

// Bounds is the range of replica counts that a Scale Proposal may set, inclusive.
type Bounds struct {
	// Min is the smallest permitted replica count.
	Min int32
	// Max is the largest permitted replica count.
	Max int32
}

// DefaultBounds returns the bounds that apply unless configured otherwise.
func DefaultBounds() Bounds { return Bounds{Min: 1, Max: 10} }

// Config configures the Scale validator.
type Config struct {
	// Reader reads Targets directly from the API server, not from a cache.
	Reader client.Reader
	// Sandbox is the repository that holds the Targets' manifests.
	Sandbox sandbox.Repository
	// AllowedNamespaces are the namespaces whose Targets may be scaled. None are allowed when it is empty.
	AllowedNamespaces []string
	// Bounds are the permitted replica counts.
	Bounds Bounds
}

// Validator validates Scale Proposals and renders their patch.
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

// Validate checks the Proposal and, when it passes, renders the patch.
func (v *Validator) Validate(ctx context.Context, req proposal.Request) (proposal.Outcome, error) {
	var params v1alpha1.ScaleParameters
	if p := req.Action.Parameters; p != nil && p.Scale != nil {
		params = *p.Scale
	} else {
		return proposal.Outcome{Errors: []agentapi.ValidationError{{
			Field: fieldPrefix, Message: "scale parameters are required",
		}}}, nil
	}
	return v.change.Validate(ctx, req, &renderer{
		replicas: params.Replicas, bounds: v.cfg.Bounds,
	})
}

// Stale reports why the Proposal's base no longer holds, or an empty string
// when it still does.
func (v *Validator) Stale(ctx context.Context, t *v1alpha1.Page) (string, error) {
	return v.change.Stale(ctx, t)
}

// renderer renders the replica-count change of one Proposal and dry-runs it.
type renderer struct {
	replicas int32
	bounds   Bounds
}

var _ change.Renderer = (*renderer)(nil)

func (r *renderer) Action() v1alpha1.Action { return v1alpha1.ActionScale }

func (r *renderer) ParametersField() string { return fieldPrefix }

func (r *renderer) CheckParameters() []agentapi.ValidationError {
	if r.replicas >= r.bounds.Min && r.replicas <= r.bounds.Max {
		return nil
	}
	return []agentapi.ValidationError{{
		Field: fieldPrefix + ".replicas",
		Message: fmt.Sprintf("%d is outside the permitted range %d to %d replicas",
			r.replicas, r.bounds.Min, r.bounds.Max),
	}}
}

func (r *renderer) Check(*change.LiveTarget) []agentapi.ValidationError { return nil }

func (r *renderer) Render(
	_ context.Context, src []byte, target v1alpha1.TargetReference,
	_ *change.LiveTarget, _ string,
) ([]byte, []agentapi.ValidationError, error) {
	rendered, err := render(src, target, r.replicas)
	return rendered, nil, err
}

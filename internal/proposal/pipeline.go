package proposal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// ExpiryAfter is how long an unapproved Proposal stays open.
const ExpiryAfter = time.Hour

// Request is one Proposal submitted for Validation.
type Request struct {
	// Page is the Page the Proposal belongs to.
	Page *v1alpha1.Page
	// Action is the Action and parameters the agent proposed.
	Action v1alpha1.ProposedAction
	// Base is the repository state the Action is rendered against. A FilePatch
	// sets it from the Target Repository's head so the writer can refuse a
	// branch that moved; other Actions record their own base.
	Base *v1alpha1.ProposalBase
	// RenderedChange is the reviewable change for an Action whose validator
	// does not render one, such as the whole-file FilePatch Action.
	RenderedChange *v1alpha1.RenderedChange
}

// Outcome is what an ActionValidator decides about a Request.
type Outcome struct {
	// Errors are the Validation errors. A Proposal is accepted only when there are none.
	Errors []agentapi.ValidationError
	// Change is the exact change to show the approver and to execute, when the Action has one.
	Change *v1alpha1.RenderedChange
	// Base is the state that Change was rendered against.
	Base *v1alpha1.ProposalBase
}

// ActionValidator performs Validation for one Action. An error return means
// Validation could not run (for example the Sandbox was unreachable), which
// is distinct from the Proposal being invalid.
type ActionValidator interface {
	Validate(ctx context.Context, req Request) (Outcome, error)
}

// ActionValidatorFunc adapts a function to an ActionValidator.
type ActionValidatorFunc func(ctx context.Context, req Request) (Outcome, error)

// Validate calls f.
func (f ActionValidatorFunc) Validate(ctx context.Context, req Request) (Outcome, error) {
	return f(ctx, req)
}

// Pipeline validates Proposals by dispatching to the validator registered for
// the Action. An Action without a registered validator is not accepted.
type Pipeline struct {
	validators map[v1alpha1.Action]ActionValidator
	clock      clock.PassiveClock
}

// NewPipeline returns a Pipeline that accepts exactly the Actions in validators.
func NewPipeline(clk clock.PassiveClock, validators map[v1alpha1.Action]ActionValidator) *Pipeline {
	registered := make(map[v1alpha1.Action]ActionValidator, len(validators))
	maps.Copy(registered, validators)
	return &Pipeline{validators: registered, clock: clk}
}

// DefaultValidators returns the validators that this version of
// pagerless-server accepts: FilePatch and NoAction.
func DefaultValidators() map[v1alpha1.Action]ActionValidator {
	return map[v1alpha1.Action]ActionValidator{
		v1alpha1.ActionFilePatch: filePatchValidator(),
		v1alpha1.ActionNoAction: ActionValidatorFunc(func(context.Context, Request) (Outcome, error) {
			return Outcome{}, nil
		}),
	}
}

// StalenessChecker is implemented by an ActionValidator whose Proposals are
// rendered against a base that can change.
type StalenessChecker interface {
	// Stale returns the reason the Page's current Proposal is a Stale
	// Proposal, or an empty string when its base still holds.
	Stale(ctx context.Context, t *v1alpha1.Page) (string, error)
}

// Stale reports why the Page's current Proposal is a Stale Proposal, or an
// empty string when it is not. A Proposal whose Action has nothing to render
// against is never stale.
func (p *Pipeline) Stale(ctx context.Context, t *v1alpha1.Page) (string, error) {
	current := t.Status.Proposal
	if current == nil {
		return "", nil
	}
	checker, ok := p.validators[current.Action].(StalenessChecker)
	if !ok {
		return "", nil
	}
	reason, err := checker.Stale(ctx, t)
	if err != nil {
		return "", fmt.Errorf("check %s proposal for staleness: %w", current.Action, err)
	}
	return reason, nil
}

// Revalidate validates the Page's current Proposal again against the
// present state and returns it under a new hash. The attempt and the expiry
// are kept, because re-validation neither gives the agent another try nor
// gives the approver more time. When Validation now finds errors it returns
// them and no Proposal.
func (p *Pipeline) Revalidate(
	ctx context.Context, t *v1alpha1.Page,
) (*v1alpha1.Proposal, []agentapi.ValidationError, error) {
	current := t.Status.Proposal
	if current == nil {
		return nil, nil, errors.New("the Page has no Proposal to re-validate")
	}
	rebuilt, errs, err := p.Build(ctx, Request{
		Page: t, Action: current.ProposedAction, Base: current.Base,
		RenderedChange: current.RenderedChange,
	}, current.Attempt)
	if err != nil || len(errs) > 0 {
		return nil, errs, err
	}
	rebuilt.ExpiresAt = current.ExpiresAt
	return rebuilt, nil, nil
}

// Build validates the Proposal and, if it passes, returns the Proposal to
// store: the agent's Action plus the rendered change, base, hash, attempt, and
// expiry. When Validation finds errors it returns them and no Proposal.
func (p *Pipeline) Build(
	ctx context.Context, req Request, attempt int32,
) (*v1alpha1.Proposal, []agentapi.ValidationError, error) {
	validator, ok := p.validators[req.Action.Action]
	if !ok {
		return nil, []agentapi.ValidationError{{
			Field:   "proposal.action",
			Message: fmt.Sprintf("action %s is not accepted by this version of pagerless-server", req.Action.Action),
		}}, nil
	}
	outcome, err := validator.Validate(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("validate %s proposal: %w", req.Action.Action, err)
	}
	if len(outcome.Errors) > 0 {
		return nil, outcome.Errors, nil
	}
	base := outcome.Base
	if base == nil {
		base = req.Base
	}
	change := outcome.Change
	if change == nil {
		change = req.RenderedChange
	}
	hash, err := Hash(req.Page, req.Action, change, base)
	if err != nil {
		return nil, nil, err
	}
	proposal := &v1alpha1.Proposal{
		ProposedAction: *req.Action.DeepCopy(),
		RenderedChange: change,
		Base:           base,
		Hash:           hash,
		Attempt:        attempt,
		ExpiresAt:      metav1.NewTime(p.clock.Now().Add(ExpiryAfter)),
	}
	switch req.Action.Action {
	case v1alpha1.ActionFilePatch:
		proposal.Kind = v1alpha1.ProposalKindFilePatch
		if req.Action.Parameters != nil && req.Action.Parameters.FilePatch != nil {
			proposal.Files = req.Action.Parameters.FilePatch.Files
		}
	case v1alpha1.ActionNoAction:
		proposal.Kind = v1alpha1.ProposalKindDiagnosisOnly
	default:
		proposal.Kind = v1alpha1.ProposalKindAction
	}
	return proposal, nil, nil
}

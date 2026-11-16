package proposal

import (
	"context"
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
	// Triage is the Triage the Proposal belongs to.
	Triage *v1alpha1.Triage
	// Action is the Action and parameters the agent proposed.
	Action v1alpha1.ProposedAction
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
// pagerless-server accepts: NoAction only.
func DefaultValidators() map[v1alpha1.Action]ActionValidator {
	return map[v1alpha1.Action]ActionValidator{
		v1alpha1.ActionNoAction: ActionValidatorFunc(func(context.Context, Request) (Outcome, error) {
			return Outcome{}, nil
		}),
	}
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
	hash, err := Hash(req.Triage, req.Action, outcome.Change)
	if err != nil {
		return nil, nil, err
	}
	return &v1alpha1.Proposal{
		ProposedAction: *req.Action.DeepCopy(),
		RenderedChange: outcome.Change,
		Base:           outcome.Base,
		Hash:           hash,
		Attempt:        attempt,
		ExpiresAt:      metav1.NewTime(p.clock.Now().Add(ExpiryAfter)),
	}, nil, nil
}

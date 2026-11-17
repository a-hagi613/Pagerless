package remediation

import (
	"context"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/agentapi"
	"github.com/a-hagi613/pagerless/internal/proposal"
)

// Restart validates RestartWorkload Proposals. The Action executes live, so its
// Proposal holds no rendered change: the base is the Workload's generation, and
// the Executor deletes the Workload's pods one at a time (ADR-0002, ADR-0008).
type Restart struct {
	change *Validator
}

// NewRestart returns the validator for RestartWorkload.
func NewRestart(cfg Config) *Restart { return &Restart{change: New(cfg)} }

var _ proposal.ActionValidator = (*Restart)(nil)
var _ proposal.StalenessChecker = (*Restart)(nil)

// Validate checks the Proposal and records the Workload generation as its base.
func (r *Restart) Validate(ctx context.Context, req proposal.Request) (proposal.Outcome, error) {
	if p := req.Action.Parameters; p != nil {
		return proposal.Outcome{Errors: []agentapi.ValidationError{{
			Field:   "proposal.parameters",
			Message: "RestartWorkload takes no parameters",
		}}}, nil
	}
	return r.change.ValidateLive(ctx, req)
}

// Stale reports why the Proposal's base no longer holds.
func (r *Restart) Stale(ctx context.Context, t *v1alpha1.Triage) (string, error) {
	return r.change.Stale(ctx, t)
}

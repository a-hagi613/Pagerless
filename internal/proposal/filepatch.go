package proposal

import (
	"context"

	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// filePatchValidator accepts a FilePatch Proposal whose files pass the
// repository path and content policy. Rendering, the OpenAPI check, and the
// pull request are Validation's later stages; this stage refuses a Proposal
// that could never be a reviewable change.
func filePatchValidator() ActionValidator {
	return ActionValidatorFunc(func(_ context.Context, req Request) (Outcome, error) {
		parameters := req.Action.Parameters
		if parameters == nil || parameters.FilePatch == nil {
			return Outcome{Errors: []agentapi.ValidationError{{
				Field:   "proposal.parameters.filePatch",
				Message: "parameters.filePatch is required for FilePatch",
			}}}, nil
		}
		errs := agentapi.CheckFilePatch("proposal.parameters.filePatch",
			parameters.FilePatch.Files, parameters.FilePatch.Confidence)
		return Outcome{Errors: errs}, nil
	})
}

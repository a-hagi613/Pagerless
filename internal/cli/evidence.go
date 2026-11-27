package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newEvidenceCommand prints one sanitized tool attempt.
func newEvidenceCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "evidence <pageId> <callId>",
		Short: "Show the sanitized arguments and result of one tool call",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 2 {
				return usagef("evidence needs a page ID and a call ID")
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.showEvidence(ctx, args[0], args[1])
		}),
	}
}

// showEvidence renders the same evidence the dashboard expands.
func (a *app) showEvidence(ctx context.Context, pageID, callID string) error {
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	record, err := api.evidence(ctx, pageID, callID)
	if err != nil {
		return err
	}
	if a.opts.json {
		return writeJSON(a.out(), record)
	}
	presentation := newPrinter(a.out())
	presentation.field("Call", sanitize(orPlaceholder(record.CallID)))
	presentation.field("Tool", sanitize(orPlaceholder(record.Tool)))
	presentation.field("Status", sanitize(orPlaceholder(record.Status)))
	presentation.field("Duration", fmt.Sprintf("%dms", record.DurationMS))
	presentation.field("Redacted", yesNo(record.Redacted))
	presentation.field("Truncated", yesNo(record.Truncated))
	presentation.section("Arguments")
	presentation.block("  ", indentJSON(record.Arguments))
	presentation.section("Result")
	presentation.block("  ", indentJSON(record.Result))
	return presentation.err
}

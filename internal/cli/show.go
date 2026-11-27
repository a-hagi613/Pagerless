package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// newShowCommand renders one conversation and its transcript.
func newShowCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <pageId>",
		Short: "Show a conversation, its messages, and citations",
		Args:  exactlyOneArgument,
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.show(ctx, args[0])
		}),
	}
}

// show prints the page the browser shows for the same ID.
func (a *app) show(ctx context.Context, pageID string) error {
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	detail, err := api.page(ctx, pageID)
	if err != nil {
		return err
	}
	if a.opts.json {
		return writeJSON(a.out(), detail)
	}
	return a.printDetail(detail)
}

// exactlyOneArgument reports a missing or extra argument as a usage error.
func exactlyOneArgument(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return usagef("expected exactly one page ID")
	}
	return nil
}

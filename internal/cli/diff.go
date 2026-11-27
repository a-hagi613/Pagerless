package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// newDiffCommand prints the page's proposed source change.
func newDiffCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <pageId>",
		Short: "Print the proposed source change and its pull request",
		Args:  exactlyOneArgument,
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.showDiff(ctx, args[0])
		}),
	}
}

// showDiff renders the proposal the publishing step would open.
func (a *app) showDiff(ctx context.Context, pageID string) error {
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	result, err := api.diff(ctx, pageID)
	if err != nil {
		return err
	}
	if a.opts.json {
		if result.Files == nil {
			result.Files = []diffFile{}
		}
		return writeJSON(a.out(), result)
	}
	presentation := newPrinter(a.out())
	if result.PRURL != "" {
		presentation.field("Pull request", presentation.style.cyan(sanitize(result.PRURL)))
	}
	if len(result.Files) == 0 {
		presentation.printf("No proposed change.\n")
		return presentation.err
	}
	for _, file := range result.Files {
		presentation.printf("\n%s\n", presentation.style.bold(presentation.style.yellow(sanitize(orPlaceholder(file.Path)))))
		presentation.diff(sanitize(file.Diff))
	}
	return presentation.err
}

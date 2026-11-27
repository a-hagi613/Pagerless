package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newListCommand lists the installation's conversations.
func newListCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List investigations, newest activity first",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usagef("list takes no arguments")
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, _ []string) error {
			return a.list(ctx)
		}),
	}
	cmd.Flags().IntVar(&a.opts.limit, "limit", 0, "show at most this many pages (0 shows all)")
	return cmd
}

// list prints the summaries the server already ordered by newest activity.
func (a *app) list(ctx context.Context) error {
	if a.opts.limit < 0 {
		return usagef("--limit must not be negative")
	}
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	pages, err := api.pages(ctx)
	if err != nil {
		return err
	}
	if pages == nil {
		pages = []pageSummary{}
	}
	if a.opts.limit > 0 && len(pages) > a.opts.limit {
		pages = pages[:a.opts.limit]
	}
	if a.opts.json {
		return writeJSON(a.out(), struct {
			Pages []pageSummary `json:"pages"`
		}{Pages: pages})
	}
	if len(pages) == 0 {
		_, err := fmt.Fprintln(a.out(), "No pages.")
		return err
	}
	writer := tabwriter.NewWriter(a.out(), 0, 4, 2, ' ', 0)
	table := &printer{writer: writer}
	table.printf("ID\tSTATUS\tUPDATED\tTITLE\n")
	for _, page := range pages {
		table.printf("%s\t%s\t%s\t%s\n",
			sanitize(page.ID), sanitize(orPlaceholder(page.Status)),
			formatAge(page.UpdatedAt), sanitize(orPlaceholder(page.Title)))
	}
	if table.err != nil {
		return table.err
	}
	return writer.Flush()
}

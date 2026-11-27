package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newStopCommand stops the active turn explicitly. Detaching never stops work.
func newStopCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <pageId>",
		Short: "Stop the page's active turn",
		Args:  exactlyOneArgument,
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.stopPage(ctx, args[0])
		}),
	}
}

// stopPage records a stop request. The server makes repeated calls safe.
func (a *app) stopPage(ctx context.Context, pageID string) error {
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	response, err := api.stopTurn(ctx, pageID)
	if err != nil {
		return err
	}
	if a.opts.json {
		return writeJSON(a.out(), response)
	}
	switch response.Status {
	case statusStopped:
		_, err = fmt.Fprintf(a.out(), "Stopped the active turn on %s.\n", sanitize(pageID))
	case statusUnknown, "":
		_, err = fmt.Fprintf(a.out(), "No active turn on %s.\n", sanitize(pageID))
	default:
		_, err = fmt.Fprintf(a.out(), "Stop status: %s\n", sanitize(response.Status))
	}
	return err
}

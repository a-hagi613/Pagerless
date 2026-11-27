package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// newFollowCommand resumes and follows one conversation's event stream.
func newFollowCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "follow <pageId>",
		Short: "Replay a conversation's events and follow new ones",
		Long: `Replays the durable event log and keeps the stream open. Without --after it
resumes after the last event this machine read, so a detached or interrupted
follow continues without losing or repeating entries. With --json each event is
one JSON object on its own line.`,
		Args: exactlyOneArgument,
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.followPage(ctx, args[0])
		}),
	}
	cmd.Flags().Int64Var(&a.opts.after, "after", -1,
		"resume after this sequence (default: the stored resume point)")
	cmd.Flags().BoolVar(&a.opts.fromStart, "from-start", false,
		"ignore the stored resume point and replay from sequence zero")
	return cmd
}

// followPage replays the durable log and keeps the stream open until the turn
// finishes or the operator detaches.
func (a *app) followPage(ctx context.Context, pageID string) error {
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	start := startingSequence(pageID, a.opts.after, a.opts.fromStart)
	if start > 0 {
		a.progressf("resuming after event %d\n", start)
	}
	outcome, err := a.consume(ctx, api, pageID, start, true)
	if err != nil {
		return err
	}
	if !outcome.Detached {
		return nil
	}
	if a.opts.json {
		a.progressf("\nDetached at event %d; the turn is still running.\n", outcome.LastSeq)
		a.progressf("  Resume: kubectl page follow %s --after %d\n", pageID, outcome.LastSeq)
		a.progressf("  Stop:   kubectl page stop %s\n", pageID)
		return nil
	}
	return a.reportDetach(pageID, "", outcome)
}

// newActivityCommand prints the trace once, without following.
func newActivityCommand(a *app) *cobra.Command {
	var after int64
	cmd := &cobra.Command{
		Use:   "activity <pageId>",
		Short: "Print a conversation's recorded tool activity",
		Long: `Prints the recorded events once, without following. With --json the output is
one document with an events array.`,
		Args: exactlyOneArgument,
		RunE: a.run(func(ctx context.Context, args []string) error {
			return a.activity(ctx, args[0], after)
		}),
	}
	cmd.Flags().Int64Var(&after, "after", 0, "start after this sequence")
	return cmd
}

// activity prints the ordered log that the browser renders as trace details.
func (a *app) activity(ctx context.Context, pageID string, after int64) error {
	if after < 0 {
		return usagef("--after must not be negative")
	}
	api, err := a.connect(ctx)
	if err != nil {
		return err
	}
	events, err := api.events(ctx, pageID, after)
	if err != nil {
		return err
	}
	if events == nil {
		events = []event{}
	}
	if a.opts.json {
		return writeJSON(a.out(), struct {
			Events []event `json:"events"`
		}{Events: events})
	}
	presentation := newPrinter(a.out())
	if len(events) == 0 {
		presentation.printf("No activity.\n")
		return presentation.err
	}
	for _, current := range events {
		writeEvent(presentation, current)
	}
	return presentation.err
}

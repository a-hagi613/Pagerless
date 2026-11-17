// Command kubectl-triage is the kubectl plugin that starts, lists, and shows
// Triages. kubectl runs it as `kubectl triage`.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/a-hagi613/pagerless/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := cli.NewRootCommand().ExecuteContext(ctx)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

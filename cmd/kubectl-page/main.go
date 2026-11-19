// Command kubectl-page is the kubectl plugin that starts, lists, and shows
// Pages. kubectl runs it as `kubectl page`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/a-hagi613/pagerless/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := cli.NewRootCommand().ExecuteContext(ctx)
	stop()
	if err != nil {
		// The root command silences cobra's own error printing so that the exit
		// code is the only contract, so the message is printed here once and the
		// documented code is returned to the shell.
		fmt.Fprintln(os.Stderr, "kubectl-page:", err)
		os.Exit(cli.ExitCode(err))
	}
}

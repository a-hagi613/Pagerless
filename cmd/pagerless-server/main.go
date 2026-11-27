// Command pagerless-server runs the Pagerless investigation service: the
// authenticated public API, the embedded dashboard, the durable transcript and
// the private agent and MCP clients.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/a-hagi613/pagerless/internal/config"
	"github.com/a-hagi613/pagerless/internal/server"
)

// version is set at link time by the image build.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pagerless-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if version != "dev" {
		cfg.Version = version
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	instance, err := server.New(ctx, cfg)
	if err != nil {
		return err
	}
	return instance.Run(ctx)
}

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// newDashboardCommand prints, and optionally opens, the browser interface that
// the same service serves.
func newDashboardCommand(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Print or open the dashboard served by the installation",
		Long: `Prints the address the installation serves the dashboard on. When the plugin
discovered the service, it keeps its local port-forward open until Ctrl-C; pass
--address to point at a long-running address instead, or --no-wait to exit
immediately.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usagef("dashboard takes no arguments")
			}
			return nil
		},
		RunE: a.run(func(ctx context.Context, _ []string) error {
			return a.dashboard(ctx)
		}),
	}
	cmd.Flags().BoolVar(&a.opts.open, "open", false, "open the address in the default browser")
	cmd.Flags().BoolVar(&a.opts.noWait, "no-wait", false,
		"print the address and close the local tunnel immediately")
	return cmd
}

// dashboard resolves the same address as the API, so a port-forward opened here
// also serves the browser. It keeps the tunnel open until Ctrl-C.
func (a *app) dashboard(ctx context.Context) error {
	address, err := a.resolveBase(ctx)
	if err != nil {
		return err
	}
	parsed, err := parseAddress(address)
	if err != nil {
		return err
	}
	url := parsed.String() + "/"
	tunneled := a.tunnel != nil
	if a.opts.json {
		if err := writeJSON(a.out(), struct {
			URL     string `json:"url"`
			Tunnel  bool   `json:"tunnel"`
			Waiting bool   `json:"waiting"`
		}{URL: url, Tunnel: tunneled, Waiting: tunneled && !a.opts.noWait}); err != nil {
			return err
		}
	} else {
		presentation := &printer{writer: a.out()}
		presentation.field("Dashboard", url)
		if tunneled {
			presentation.field("Tunnel", "local port-forward; it closes when this command exits")
		}
		if err := presentation.err; err != nil {
			return err
		}
	}
	if a.opts.open {
		if err := openBrowser(url); err != nil {
			a.progressf("could not open a browser: %v\n", err)
		}
	}
	if !tunneled || a.opts.noWait {
		if tunneled {
			a.progressf("The tunnel closes now; use --address or omit --no-wait to keep it open.\n")
		}
		return nil
	}
	a.progressf("Serving the dashboard through a local tunnel; press Ctrl-C to close it.\n")
	<-ctx.Done()
	return nil
}

// openBrowser starts the platform's URL opener without a shell.
func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start %s: %w", strings.Join(command.Args, " "), err)
	}
	go func() { _ = command.Wait() }()
	return nil
}

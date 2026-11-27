package cli

import (
	"context"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, set at link time by GoReleaser. A developer build reports
// its placeholders, which makes it obvious that no release produced it.
var (
	// Version is the release version, such as 1.0.0.
	Version = "dev"
	// Commit is the commit the binary was built from.
	Commit = "none"
	// BuildDate is when the binary was built, in RFC 3339 form.
	BuildDate = "unknown"
	// ExpectedChartVersion is the chart version that this CLI is released with.
	ExpectedChartVersion = "0.0.0"
)

// versionInfo is the version command's JSON payload.
type versionInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildDate     string `json:"buildDate"`
	ExpectedChart string `json:"expectedChartVersion"`
	GoVersion     string `json:"goVersion"`
	API           string `json:"api"`
}

// newVersionCommand prints local build metadata. The server version is not
// part of the contract, so this command makes no authenticated call.
func newVersionCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin build metadata",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usagef("version takes no arguments")
			}
			return nil
		},
		RunE: a.run(func(_ context.Context, _ []string) error {
			info := versionInfo{
				Version:       Version,
				Commit:        Commit,
				BuildDate:     BuildDate,
				ExpectedChart: ExpectedChartVersion,
				GoVersion:     runtime.Version(),
				API:           "/api/v1",
			}
			if a.opts.json {
				return writeJSON(a.out(), info)
			}
			_, err := fmt.Fprintf(a.out(),
				"kubectl-page %s\ncommit %s\nbuilt %s\nexpected chart %s\ngo %s\napi %s\n",
				info.Version, info.Commit, info.BuildDate, info.ExpectedChart, info.GoVersion, info.API)
			return err
		}),
	}
}

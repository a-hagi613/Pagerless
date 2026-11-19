package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, set at link time by GoReleaser. A developer build reports
// its placeholders, which makes it obvious that no release produced it.
var (
	// Version is the release version, such as 2.0.0.
	Version = "dev"
	// Commit is the commit the binary was built from.
	Commit = "none"
	// BuildDate is when the binary was built, in RFC 3339 form.
	BuildDate = "unknown"
	// ExpectedChartVersion is the chart version that this CLI is released with.
	ExpectedChartVersion = "2.0.0"
)

// newVersionCommand returns the command that prints the build and the chart
// version the binary expects.
func newVersionCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit, and expected chart version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionInfo{
				Version:       Version,
				Commit:        Commit,
				BuildDate:     BuildDate,
				ExpectedChart: ExpectedChartVersion,
				GoVersion:     runtime.Version(),
			}
			if output == formatJSON {
				return writeJSON(cmd.OutOrStdout(), &info)
			}
			out := cmd.OutOrStdout()
			_, err := fmt.Fprintf(out, "kubectl-page %s\ncommit %s\nbuilt %s\nexpected chart %s\ngo %s\n",
				info.Version, info.Commit, info.BuildDate, info.ExpectedChart, info.GoVersion)
			return err
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json")
	return cmd
}

// versionInfo is the version command's JSON and table payload.
type versionInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	BuildDate     string `json:"buildDate"`
	ExpectedChart string `json:"expectedChartVersion"`
	GoVersion     string `json:"goVersion"`
}

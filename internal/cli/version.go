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

// newVersionCommand returns the command that prints the build, the chart
// version the binary expects, and the transport that a command would use.
func newVersionCommand(opts *options) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit, expected chart version, and mode",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := versionInfo{
				Version:       Version,
				Commit:        Commit,
				BuildDate:     BuildDate,
				ExpectedChart: ExpectedChartVersion,
				GoVersion:     runtime.Version(),
				Mode:          opts.mode,
			}
			if opts.mode == modeServer {
				client, err := newAPIClient(opts.server, "")
				if err != nil {
					return err
				}
				stored, err := loadConfig(opts.configPath)
				if err != nil {
					return err
				}
				if stored.SessionToken != "" {
					client.token = stored.SessionToken
					if csrf, err := csrfFromSession(stored.SessionToken); err == nil {
						client.csrf = csrf
					}
				}
				var server serverVersion
				if err := client.do(cmd.Context(), "GET", "/api/version", nil, &server); err != nil {
					return err
				}
				info.ServerVersion = server.Version
				info.ServerMode = server.Mode
			}
			if output == formatJSON {
				return writeJSON(cmd.OutOrStdout(), &info)
			}
			out := cmd.OutOrStdout()
			_, err := fmt.Fprintf(out,
				"kubectl-page %s\ncommit %s\nbuilt %s\nexpected chart %s\ngo %s\nmode %s\n",
				info.Version, info.Commit, info.BuildDate, info.ExpectedChart, info.GoVersion, info.Mode)
			if err != nil {
				return err
			}
			if info.ServerVersion != "" {
				_, err = fmt.Fprintf(out, "server %s (auth.mode %s)\n", info.ServerVersion, info.ServerMode)
			}
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
	Mode          string `json:"mode"`
	ServerVersion string `json:"serverVersion,omitempty"`
	ServerMode    string `json:"serverMode,omitempty"`
}

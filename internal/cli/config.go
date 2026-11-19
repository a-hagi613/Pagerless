package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/internal/credentials"
)

// newConfigCommand returns the command group that reads and writes the
// credentials Secret.
func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and write the credentials Secret",
		Long: `Read the masked settings, write one section of them, or clear one key.

A credential is never a flag. Supply it from an interactive prompt without
echo, from standard input, from a file, or from an env file, so it never reaches
shell history or a process listing.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return usagef("use config get, config set, or config unset")
		},
	}
	cmd.AddCommand(newConfigGetCommand(opts), newConfigSetCommand(opts), newConfigUnsetCommand(opts))
	return cmd
}

// newConfigGetCommand returns the command that prints the masked settings.
func newConfigGetCommand(opts *options) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "Print the masked settings, fingerprints, and external ownership",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			view, err := s.settings(cmd.Context())
			if err != nil {
				return err
			}
			switch format {
			case formatJSON:
				return writeJSON(cmd.OutOrStdout(), view)
			case formatYAML:
				return writeYAML(cmd.OutOrStdout(), view)
			default:
				return writeSettings(cmd.OutOrStdout(), view)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	return cmd
}

// writeSettings prints the masked settings as a table.
func writeSettings(w io.Writer, view serverSettings) error {
	p := &printer{w: w}
	p.field("Managed by", view.ManagedBy)
	p.field("Manager", view.Manager)
	if view.WriteCredentials {
		p.field("Write", "allowed")
	} else {
		p.field("Write", "read-only: the Secret is managed externally")
	}
	if view.Onboarding {
		p.field("Onboarding", "required: no AI endpoint or Target Repository is configured yet")
	}
	p.printf("\n%-28s %-10s %-20s %s\n", "KEY", "CONFIGURED", "FINGERPRINT", "UPDATED")
	keys := make([]string, 0, len(view.Fields))
	for key := range view.Fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		field := view.Fields[key]
		shown := "no"
		if field.Configured {
			shown = "yes"
		}
		updated := "-"
		if field.UpdatedAt != nil {
			updated = field.UpdatedAt.Format(time.RFC3339)
		}
		fingerprint := field.Fingerprint
		if fingerprint == "" {
			fingerprint = "-"
		}
		p.printf("%-28s %-10s %-20s %s\n", key, shown, fingerprint, updated)
	}
	if value := view.Values[credentials.KeyAIBaseURL]; value != "" {
		p.printf("\n%s %s\n", credentials.KeyAIBaseURL+":", value)
	}
	if value := view.Values[credentials.KeyAIModel]; value != "" {
		p.printf("%s %s\n", credentials.KeyAIModel+":", value)
	}
	if value := view.Values[credentials.KeyGitHubRepo]; value != "" {
		p.printf("%s %s\n", credentials.KeyGitHubRepo+":", value)
	}
	if value := view.Values[credentials.KeyGitHubAPIURL]; value != "" {
		p.printf("%s %s\n", credentials.KeyGitHubAPIURL+":", value)
	}
	return p.err
}

// newConfigSetCommand returns the command that writes one section.
func newConfigSetCommand(opts *options) *cobra.Command {
	var (
		source       sourceFlags
		aiBaseURL    string
		aiModel      string
		githubRepo   string
		githubAPIURL string
		connector    string
	)
	cmd := &cobra.Command{
		Use:   "set ai|github|connector|server",
		Short: "Write the AI endpoint, the Target Repository, a connector token, or the server session",
		Long: `Write one section of the credentials Secret, merging only the keys that are
given.

  config set ai --ai-base-url <url> --ai-model <model>
  config set github --github-repo <owner/name>
  config set connector --name <name>
  config set server --server <url>

A secret value is never a flag. The API key, the repository token, the connector
token, and the bootstrap token are read from an interactive prompt without echo,
from standard input, from a file, or from an env file.`,
		Args: exactlyOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := source.validate(); err != nil {
				return err
			}
			values := map[string]string{}
			var wanted []string
			switch args[0] {
			case "ai":
				putIfSet(values, credentials.KeyAIBaseURL, aiBaseURL)
				putIfSet(values, credentials.KeyAIModel, aiModel)
				wanted = []string{credentials.KeyAIAPIKey}
			case "github":
				putIfSet(values, credentials.KeyGitHubRepo, githubRepo)
				putIfSet(values, credentials.KeyGitHubAPIURL, githubAPIURL)
				wanted = []string{credentials.KeyGitHubToken}
			case "connector":
				if strings.TrimSpace(connector) == "" {
					return usagef("config set connector needs --name <name>")
				}
				wanted = []string{credentials.ConnectorTokenKey(connector)}
			case "server":
				return runLogin(cmd, opts, source)
			default:
				return usagef("unknown section %q: use ai, github, connector, or server", args[0])
			}
			// A source or a terminal supplies the secret; flags alone update only
			// the non-secret configuration, which is never a credential.
			if source.chosen() || (len(values) == 0 && stdinIsTerminal(cmd.InOrStdin())) {
				secrets, err := secretValues(cmd, source, wanted)
				if err != nil {
					return err
				}
				for key, value := range secrets {
					if strings.TrimSpace(value) != "" {
						values[key] = value
					}
				}
			}
			if len(values) == 0 {
				return usagef("no setting was given for %q", args[0])
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			view, err := s.writeSettings(cmd.Context(), values)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %d credential key(s) to Secret %q\n",
				len(values), view.Manager)
			return err
		},
	}
	source.register(cmd)
	cmd.Flags().StringVar(&aiBaseURL, "ai-base-url", "", "OpenAI-compatible endpoint base URL")
	cmd.Flags().StringVar(&aiModel, "ai-model", "", "model identifier the agent requests")
	cmd.Flags().StringVar(&githubRepo, "github-repo", "", "Target Repository as owner/name")
	cmd.Flags().StringVar(&githubAPIURL, "github-api-url", "", "GitHub API base URL, for GitHub Enterprise")
	cmd.Flags().StringVar(&connector, "name", "", "connector name whose token is written")
	return cmd
}

// newConfigUnsetCommand returns the command that clears one key.
func newConfigUnsetCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "unset <KEY>",
		Short: "Clear one credential key without touching the others",
		Args:  exactlyOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.ToUpper(strings.TrimSpace(args[0]))
			if !credentials.SecretKey(key) {
				return usagef("unknown credential key %q: use a fixed upper-snake key or CONNECTOR_<NAME>_TOKEN", args[0])
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			if _, err := s.clearSetting(cmd.Context(), key); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "cleared %s\n", key)
			return err
		},
	}
}

// putIfSet copies a non-empty value into the flat key map.
func putIfSet(values map[string]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	values[key] = value
}

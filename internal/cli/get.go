package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newGetCommand returns the command that shows one Page. `show` is an accepted
// synonym, so the older spelling keeps working.
func newGetCommand(opts *options) *cobra.Command {
	var (
		output     string
		watchSteps bool
	)
	cmd := &cobra.Command{
		Use:     "get <name>",
		Aliases: []string{"show"},
		Short:   "Show a Page's Diagnosis, Findings, Proposal, pull request, Verification checks, and Narrative",
		Args:    exactlyOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			if watchSteps && format != formatDefault {
				return usagef("--watch prints the presentation of the Page and cannot be combined with --output")
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			if watchSteps && s.api != nil {
				return usagef("--watch streams Page steps and is available in kubeconfig mode only")
			}
			out := cmd.OutOrStdout()
			if watchSteps {
				key := client.ObjectKey{Namespace: opts.pageNamespace, Name: args[0]}
				page, err := follow(cmd.Context(), s.clus.client, key, out)
				if err != nil {
					return err
				}
				rec := recordFromPage(page)
				p := &printer{w: out}
				p.printf("\n")
				if p.err != nil {
					return p.err
				}
				return describe(out, rec)
			}
			rec, err := s.page(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			switch format {
			case formatJSON:
				return writeJSON(out, rec.Raw)
			case formatYAML:
				return writeYAML(out, rec.Raw)
			default:
				return describe(out, rec)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	cmd.Flags().BoolVar(&watchSteps, "watch", false,
		"print the agent's steps as they happen, then the presentation, once the Page is Proposed or ends")
	return cmd
}

// pageError describes a failure to read a Page, naming the Page when it does not exist.
func pageError(err error, name, namespace string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("page %q not found in namespace %q", name, namespace)
	}
	if _, ok := errors.AsType[*ExitError](err); ok {
		return err
	}
	return fmt.Errorf("get Page %q: %w", name, err)
}

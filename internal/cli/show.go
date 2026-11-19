package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// newShowCommand returns the command that shows one Page.
func newShowCommand(conn *connection) *cobra.Command {
	var (
		output     string
		watchSteps bool
	)
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a Page's Diagnosis, Findings, Proposal, pull request, Verification checks, and Narrative",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			if watchSteps && format != formatDefault {
				return errors.New("--watch prints the presentation of the Page and cannot be combined with --output")
			}
			cl, err := conn.connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			name := args[0]
			key := client.ObjectKey{Namespace: PageNamespace, Name: name}
			out := cmd.OutOrStdout()

			if watchSteps {
				tr, err := follow(ctx, cl.client, key, out)
				if err != nil {
					return err
				}
				p := &printer{w: out}
				p.printf("\n")
				if p.err != nil {
					return p.err
				}
				return describe(out, tr)
			}

			var tr v1alpha1.Page
			if err := cl.client.Get(ctx, key, &tr); err != nil {
				return pageError(err, name)
			}
			tr.TypeMeta = metaFor("Page")
			switch format {
			case formatJSON:
				return writeJSON(out, &tr)
			case formatYAML:
				return writeYAML(out, &tr)
			default:
				return describe(out, &tr)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	cmd.Flags().BoolVar(&watchSteps, "watch", false,
		"print the agent's steps as they happen, then the presentation, once the Page is Proposed or ends")
	return cmd
}

// pageError describes a failure to read a Page, naming the Page when it does not exist.
func pageError(err error, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("no Page named %q in namespace %q", name, PageNamespace)
	}
	return fmt.Errorf("get Page %q: %w", name, err)
}

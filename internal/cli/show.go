package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// newShowCommand returns the command that shows one Triage.
func newShowCommand(conn *connection) *cobra.Command {
	var (
		output     string
		watchSteps bool
	)
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a Triage's Diagnosis, Findings, Proposal, execution, Verification checks, and Narrative",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			if watchSteps && format != formatDefault {
				return errors.New("--watch prints the presentation of the Triage and cannot be combined with --output")
			}
			cl, err := conn.connect()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			name := args[0]
			key := client.ObjectKey{Namespace: TriageNamespace, Name: name}
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

			var tr v1alpha1.Triage
			if err := cl.client.Get(ctx, key, &tr); err != nil {
				return triageError(err, name)
			}
			tr.TypeMeta = metaFor("Triage")
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
		"print the agent's steps as they happen, then the presentation, once the Triage is Proposed or ends")
	return cmd
}

// triageError describes a failure to read a Triage, naming the Triage when it does not exist.
func triageError(err error, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("no Triage named %q in namespace %q", name, TriageNamespace)
	}
	return fmt.Errorf("get Triage %q: %w", name, err)
}

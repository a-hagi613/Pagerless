package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// dismissOptions holds what the dismiss command needs from the command line.
type dismissOptions struct {
	conn   *connection
	reason string
	by     string
}

func newDismissCommand(conn *connection) *cobra.Command {
	o := &dismissOptions{conn: conn}
	cmd := &cobra.Command{
		Use:   "dismiss <page> --reason <reason>",
		Short: "Reject the Proposal of a Page",
		Long: `Reject a Page's Proposal by writing decision annotations that carry the
Rejection and its reason, bound to the Proposal's hash. A Rejection is final:
the Page ends as Rejected, and a new attempt is a new Page, optionally with
a hint.`,
		Args: cobra.ExactArgs(1),
		RunE: o.run,
	}
	cmd.Flags().StringVar(&o.reason, "reason", "", "why the Proposal is rejected (required)")
	cmd.Flags().StringVar(&o.by, "by", "", "identity to record as the approver (default: the kubeconfig user)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (o *dismissOptions) run(cmd *cobra.Command, args []string) error {
	reason := strings.TrimSpace(o.reason)
	if reason == "" {
		return errors.New("a rejection needs a reason: pass --reason with a non-empty text")
	}
	if len(reason) > v1alpha1.MaxReasonLength {
		return fmt.Errorf("the reason is %d characters long; at most %d are allowed", len(reason), v1alpha1.MaxReasonLength)
	}
	cl, err := o.conn.connect()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	page, err := loadProposedPage(ctx, cl.client, args[0])
	if err != nil {
		return err
	}
	decidedBy := o.by
	if decidedBy == "" {
		decidedBy = authenticatedUser(ctx, cl.client)
	}
	if err := submitDecision(ctx, cl.client, page, v1alpha1.DecisionRejected, reason, decidedBy); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s dismissed by %s\n", pageRef(page.Name), decidedBy)
	return err
}

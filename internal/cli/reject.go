package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// rejectOptions holds what the reject command needs from the command line.
type rejectOptions struct {
	conn   *connection
	reason string
}

func newRejectCommand(conn *connection) *cobra.Command {
	o := &rejectOptions{conn: conn}
	cmd := &cobra.Command{
		Use:   "reject <triage> --reason <reason>",
		Short: "Reject the Proposal of a Triage",
		Long: `Reject a Triage's Proposal by creating a TriageApproval that carries the
Rejection and its reason, bound to the Proposal's hash. A Rejection is final:
the Triage ends as Rejected, and a new attempt is a new Triage, optionally with
a hint.`,
		Args: cobra.ExactArgs(1),
		RunE: o.run,
	}
	cmd.Flags().StringVar(&o.reason, "reason", "", "why the Proposal is rejected (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (o *rejectOptions) run(cmd *cobra.Command, args []string) error {
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
	tr, err := loadProposedTriage(ctx, cl.client, args[0])
	if err != nil {
		return err
	}
	approval, err := submitDecision(ctx, cl.client, tr, v1alpha1.DecisionRejected, reason)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s created (rejected by %s)\n",
		approvalRef(approval.Name), approval.Spec.Approver)
	return err
}

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// rejectOptions holds what the reject command needs from the command line.
type rejectOptions struct {
	opts   *options
	reason string
	by     string
}

// newRejectCommand returns the command that rejects a Proposal. `dismiss` is an
// accepted synonym, so the Portal's wording keeps working.
func newRejectCommand(opts *options, use string, hidden bool) *cobra.Command {
	o := &rejectOptions{opts: opts}
	cmd := &cobra.Command{
		Use:    use + " <page> --reason <reason>",
		Hidden: hidden,
		Short:  "Reject the Proposal of a Page",
		Long: `Reject a Page's Proposal by recording a rejection bound to the Proposal's
hash. A rejection is final: the Page ends as Rejected, and a new attempt is a
new Page, optionally with a hint.`,
		Args: exactlyOneArg,
		RunE: o.run,
	}
	cmd.Flags().StringVar(&o.reason, "reason", "", "why the Proposal is rejected (required)")
	cmd.Flags().StringVar(&o.by, "by", "",
		"identity to record in kubeconfig mode (default: the kubeconfig user; server mode records the session user)")
	return cmd
}

func (o *rejectOptions) run(cmd *cobra.Command, args []string) error {
	reason := strings.TrimSpace(o.reason)
	if reason == "" {
		return usagef("a rejection needs a reason: pass --reason with a non-empty text")
	}
	if len(reason) > v1alpha1.MaxReasonLength {
		return usagef("the reason is %d characters long; at most %d are allowed", len(reason), v1alpha1.MaxReasonLength)
	}
	s, err := openSession(cmd, o.opts)
	if err != nil {
		return err
	}
	rec, err := s.page(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if rec.Phase != v1alpha1.PhaseProposed || rec.Proposal == nil {
		return fmt.Errorf("page %s is in phase %s; only a Proposed Page accepts a decision",
			rec.Name, phaseLabel(rec.Phase))
	}
	if s.api != nil && o.by != "" {
		return usagef("--by is a kubeconfig-mode flag; server mode records the signed-in user")
	}
	decidedBy, err := s.decide(cmd.Context(), rec, v1alpha1.DecisionRejected, reason, o.by)
	if err != nil {
		return err
	}
	if decidedBy == "" {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s rejected\n", pageRef(rec.Name))
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s rejected by %s\n", pageRef(rec.Name), decidedBy)
	return err
}

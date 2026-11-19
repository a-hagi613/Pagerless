package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// errNotConfirmed is returned when the approver does not confirm the change.
var errNotConfirmed = errors.New("not approved: the change was not confirmed")

// approveOptions holds what the approve command needs from the command line.
type approveOptions struct {
	opts   *options
	yes    bool
	reason string
	by     string
}

// newApproveCommand returns the command that approves one Proposal.
func newApproveCommand(opts *options) *cobra.Command {
	o := &approveOptions{opts: opts}
	cmd := &cobra.Command{
		Use:   "approve <page>",
		Short: "Approve the Proposal of a Page, binding the decision to its hash",
		Long: `Show the rendered change of a Page's Proposal and, after confirmation, approve
exactly that Proposal.

The decision records the Proposal hash that was displayed. If the Proposal
changes before the decision is recorded, the decision is refused with exit code
4 and nothing is approved. Approving a Proposal that changes no file
acknowledges the Diagnosis.`,
		Args: exactlyOneArg,
		RunE: o.run,
	}
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "approve without asking for confirmation")
	cmd.Flags().StringVar(&o.reason, "reason", "", "optional reason to record with the approval")
	cmd.Flags().StringVar(&o.by, "by", "",
		"identity to record in kubeconfig mode (default: the kubeconfig user; server mode records the session user)")
	return cmd
}

func (o *approveOptions) run(cmd *cobra.Command, args []string) error {
	if len(o.reason) > v1alpha1.MaxReasonLength {
		return usagef("the reason is %d characters long; at most %d are allowed", len(o.reason), v1alpha1.MaxReasonLength)
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
	out := cmd.OutOrStdout()
	if err := renderProposal(out, rec); err != nil {
		return err
	}
	if !o.yes {
		confirmed, err := confirm(out, cmd.InOrStdin(), approvalPrompt(rec.Proposal))
		if err != nil {
			return err
		}
		if !confirmed {
			return errNotConfirmed
		}
	}
	decidedBy := o.by
	if s.api != nil && decidedBy != "" {
		return usagef("--by is a kubeconfig-mode flag; server mode records the signed-in user")
	}
	decidedBy, err = s.decide(cmd.Context(), rec, v1alpha1.DecisionApproved, o.reason, decidedBy)
	if err != nil {
		return err
	}
	if decidedBy == "" {
		_, err = fmt.Fprintf(out, "%s approved\n", pageRef(rec.Name))
		return err
	}
	_, err = fmt.Fprintf(out, "%s approved by %s\n", pageRef(rec.Name), decidedBy)
	return err
}

// proposalTargetLabel names a Target as "Kind namespace/name", or "Kind name"
// for the cluster-scoped Node.
func proposalTargetLabel(ref v1alpha1.TargetReference) string {
	if ref.Namespace == "" {
		return string(ref.Kind) + " " + ref.Name
	}
	return string(ref.Kind) + " " + ref.Namespace + "/" + ref.Name
}

// approvalPrompt asks the question that matches the Proposal's kind.
func approvalPrompt(p *v1alpha1.Proposal) string {
	if p.Action == v1alpha1.ActionNoAction {
		return "Acknowledge this Diagnosis? [y/N]: "
	}
	return "Approve this change? [y/N]: "
}

// confirm asks the question and reads one line. Anything but y or yes is a
// refusal, including an input that ends without an answer.
func confirm(out io.Writer, in io.Reader, question string) (bool, error) {
	if _, err := fmt.Fprint(out, question); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read the confirmation: %w", err)
	}
	if !strings.HasSuffix(line, "\n") {
		// The input ended without a newline, so end the prompt's line ourselves.
		if _, err := fmt.Fprintln(out); err != nil {
			return false, err
		}
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// renderProposal shows what an approver decides on: the Diagnosis, the
// Proposal, and the exact rendered change with the base it applies to.
func renderProposal(w io.Writer, rec *record) error {
	proposal := rec.Proposal
	var b strings.Builder
	field := func(label, value string) { fmt.Fprintf(&b, "%-10s %s\n", label+":", value) }
	field("Page", rec.Name)
	field("Target", proposalTargetLabel(rec.Target))
	field("Diagnosis", rec.Diagnosis)
	field("Action", string(proposal.Action))
	if summary := parametersSummary(proposal.Parameters); summary != "" {
		field("Parameters", summary)
	}
	switch {
	case proposal.RenderedChange != nil && proposal.RenderedChange.Patch != "":
		field("File", proposal.RenderedChange.Path)
		if proposal.Base != nil {
			field("Base", fmt.Sprintf("Target Repository commit %s, Target generation %d",
				proposal.Base.Commit, proposal.Base.TargetGeneration))
		}
		b.WriteString("\nChange:\n")
		for line := range strings.SplitSeq(strings.TrimRight(proposal.RenderedChange.Patch, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("\n")
	case proposal.Action == v1alpha1.ActionNoAction:
		b.WriteString("\nThis Proposal makes no change. Approving it acknowledges the Diagnosis.\n\n")
	default:
		b.WriteString("\nThis Proposal has no rendered change.\n\n")
	}
	field("Hash", proposal.Hash)
	field("Expires", proposal.ExpiresAt.UTC().Format(time.RFC3339))
	_, err := io.WriteString(w, b.String())
	return err
}

// parametersSummary describes an Action's parameters on one line.
func parametersSummary(params *v1alpha1.ActionParameters) string {
	if params == nil {
		return ""
	}
	switch {
	case params.PatchResources != nil:
		patch := params.PatchResources
		parts := []string{"container " + patch.Container}
		if s := quantitiesSummary(patch.Requests); s != "" {
			parts = append(parts, "requests "+s)
		}
		if s := quantitiesSummary(patch.Limits); s != "" {
			parts = append(parts, "limits "+s)
		}
		return strings.Join(parts, ", ")
	case params.Scale != nil:
		return fmt.Sprintf("replicas %d", params.Scale.Replicas)
	case params.Rollback != nil:
		return "revert commit " + params.Rollback.Commit
	default:
		return ""
	}
}

// quantitiesSummary formats a resource list in name order.
func quantitiesSummary(list corev1.ResourceList) string {
	names := make([]string, 0, len(list))
	for name := range list {
		names = append(names, string(name))
	}
	slices.Sort(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		quantity := list[corev1.ResourceName(name)]
		pairs = append(pairs, name+"="+quantity.String())
	}
	return strings.Join(pairs, " ")
}

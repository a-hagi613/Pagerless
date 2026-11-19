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
	conn   *connection
	yes    bool
	reason string
	by     string
}

func newApproveCommand(conn *connection) *cobra.Command {
	o := &approveOptions{conn: conn}
	cmd := &cobra.Command{
		Use:   "approve <page>",
		Short: "Approve the Proposal of a Page",
		Long: `Show the rendered change of a Page's Proposal and, after confirmation, approve
exactly that Proposal by creating a PageApproval bound to its hash.

If the Proposal changes before the approval is admitted, the approval is
refused and nothing is approved. Approving a NoAction Proposal acknowledges the
Diagnosis.`,
		Args: cobra.ExactArgs(1),
		RunE: o.run,
	}
	cmd.Flags().BoolVarP(&o.yes, "yes", "y", false, "approve without asking for confirmation")
	cmd.Flags().StringVar(&o.reason, "reason", "", "optional reason to record with the approval")
	cmd.Flags().StringVar(&o.by, "by", "", "identity to record as the approver (default: the kubeconfig user)")
	return cmd
}

func (o *approveOptions) run(cmd *cobra.Command, args []string) error {
	if len(o.reason) > v1alpha1.MaxReasonLength {
		return fmt.Errorf("the reason is %d characters long; at most %d are allowed", len(o.reason), v1alpha1.MaxReasonLength)
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
	out := cmd.OutOrStdout()
	if err := renderProposal(out, page); err != nil {
		return err
	}
	if !o.yes {
		confirmed, err := confirm(out, cmd.InOrStdin(), approvalPrompt(page.Status.Proposal))
		if err != nil {
			return err
		}
		if !confirmed {
			return errNotConfirmed
		}
	}
	decidedBy := o.by
	if decidedBy == "" {
		decidedBy = authenticatedUser(ctx, cl.client)
	}
	if err := submitDecision(ctx, cl.client, page, v1alpha1.DecisionApproved, o.reason, decidedBy); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s approved by %s\n", pageRef(page.Name), decidedBy)
	return err
}

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
func renderProposal(w io.Writer, tr *v1alpha1.Page) error {
	p := tr.Status.Proposal
	var b strings.Builder
	field := func(label, value string) { fmt.Fprintf(&b, "%-10s %s\n", label+":", value) }
	field("Page", tr.Name)
	field("Target", fmt.Sprintf("%s %s/%s", tr.Spec.Target.Kind, tr.Spec.Target.Namespace, tr.Spec.Target.Name))
	field("Diagnosis", tr.Status.Diagnosis)
	field("Action", string(p.Action))
	if summary := parametersSummary(p.Parameters); summary != "" {
		field("Parameters", summary)
	}
	switch {
	case p.RenderedChange != nil && p.RenderedChange.Patch != "":
		field("File", p.RenderedChange.Path)
		if p.Base != nil {
			field("Base", fmt.Sprintf("Sandbox commit %s, Target generation %d", p.Base.Commit, p.Base.TargetGeneration))
		}
		b.WriteString("\nChange:\n")
		for line := range strings.SplitSeq(strings.TrimRight(p.RenderedChange.Patch, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("\n")
	case p.Action == v1alpha1.ActionNoAction:
		b.WriteString("\nThis Proposal makes no change. Approving it acknowledges the Diagnosis.\n\n")
	default:
		b.WriteString("\nThis Proposal has no rendered change.\n\n")
	}
	field("Hash", p.Hash)
	field("Expires", p.ExpiresAt.UTC().Format(time.RFC3339))
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

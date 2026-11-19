package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// newDiffCommand returns the command that prints the Proposal's rendered change
// against the base it applies to.
func newDiffCommand(opts *options) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "diff <name>",
		Short: "Show a Page's Proposal as a unified diff against the commit it applies to",
		Long: `Print the Proposal's rendered change together with the base it applies to.
The approval binds to the same Proposal hash, so the diff and the decision can
never describe different changes.`,
		Args: exactlyOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			rec, err := s.page(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rec.Proposal == nil {
				return fmt.Errorf("page %s has no Proposal to show", rec.Name)
			}
			switch format {
			case formatJSON:
				return writeJSON(cmd.OutOrStdout(), rec.Proposal)
			case formatYAML:
				return writeYAML(cmd.OutOrStdout(), rec.Proposal)
			default:
				return writeDiff(cmd.OutOrStdout(), rec)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	return cmd
}

// writeDiff prints the Proposal's header and its unified diff.
func writeDiff(w io.Writer, rec *record) error {
	proposal := rec.Proposal
	p := &printer{w: w}
	p.field("Page", rec.Name)
	p.field("Target", targetLabel(rec.Target))
	p.field("Action", string(proposal.Action))
	p.field("Hash", proposal.Hash)
	p.field("Expires", proposal.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"))
	if base := proposal.Base; base != nil && (base.Commit != "" || base.TargetGeneration != 0) {
		parts := make([]string, 0, 2)
		if base.Commit != "" {
			parts = append(parts, "commit "+base.Commit)
		}
		if base.TargetGeneration != 0 {
			parts = append(parts, "Target generation "+strconv.FormatInt(base.TargetGeneration, 10))
		}
		p.field("Base", strings.Join(parts, ", "))
	}
	if change := proposal.RenderedChange; change != nil {
		if change.Path != "" {
			p.field("Path", change.Path)
		}
	}
	if proposal.Action == v1alpha1.ActionNoAction ||
		proposal.RenderedChange == nil || proposal.RenderedChange.Patch == "" {
		p.printf("\nThis Proposal records a Diagnosis and changes no file.\n")
		return p.err
	}
	p.printf("\n")
	p.block("", proposal.RenderedChange.Patch)
	return p.err
}

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// pagePhases is every phase that a Page can hold, in lifecycle order. It is
// the CRD's Phase enum, and it is what --phase accepts.
var pagePhases = []v1alpha1.Phase{
	v1alpha1.PhasePending,
	v1alpha1.PhaseInvestigating,
	v1alpha1.PhaseProposed,
	v1alpha1.PhaseApproved,
	v1alpha1.PhasePullRequestOpen,
	v1alpha1.PhaseVerifying,
	v1alpha1.PhaseRecovered,
	v1alpha1.PhaseNotRecovered,
	v1alpha1.PhaseDiagnosed,
	v1alpha1.PhaseRejected,
	v1alpha1.PhaseExpired,
	v1alpha1.PhaseInvalid,
	v1alpha1.PhaseSuperseded,
	v1alpha1.PhaseFailed,
}

// phaseNames returns the accepted phases as one comma-separated string.
func phaseNames() string {
	names := make([]string, 0, len(pagePhases))
	for _, phase := range pagePhases {
		names = append(names, string(phase))
	}
	return strings.Join(names, ", ")
}

// parsePhase validates a --phase value. An empty value means no filter.
func parsePhase(raw string) (v1alpha1.Phase, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	for _, phase := range pagePhases {
		if string(phase) == value {
			return phase, nil
		}
	}
	return "", usagef("unknown phase %q: use one of %s", value, phaseNames())
}

// newListCommand returns the command that lists the Pages, most recent first.
// The columns are the Page resource's own printer columns, so the CLI and
// `kubectl get pages` cannot disagree.
func newListCommand(opts *options) *cobra.Command {
	var output, phase string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Pages, most recent first, with the resource's printer columns",
		Long: `List Pages, most recent first, with the Page resource's own printer columns.

--phase narrows the list to one phase. The accepted phases are:

  ` + phaseNames() + `

An unknown value is a usage error.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			wanted, err := parsePhase(phase)
			if err != nil {
				return err
			}
			s, err := openSession(cmd, opts)
			if err != nil {
				return err
			}
			records, raw, err := s.pages(cmd.Context(), wanted)
			if err != nil {
				return err
			}
			return writeList(cmd.OutOrStdout(), format, records, raw, opts.pageNamespace, wanted)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	cmd.Flags().StringVar(&phase, "phase", "",
		"only Pages in this phase: "+phaseNames())
	return cmd
}

// writeList writes the Pages as a table, or as JSON or YAML.
func writeList(w io.Writer, format string, records []*record, raw any, namespace string, phase v1alpha1.Phase) error {
	switch format {
	case formatJSON:
		return writeJSON(w, raw)
	case formatYAML:
		return writeYAML(w, raw)
	}
	if len(records) == 0 {
		if phase != "" {
			_, err := fmt.Fprintf(w, "No Pages found in namespace %q in phase %s.\n", namespace, phase)
			return err
		}
		_, err := fmt.Fprintf(w, "No Pages found in namespace %q.\n", namespace)
		return err
	}
	return writeTable(w, records)
}

// writeTable writes one row per Page.
func writeTable(w io.Writer, records []*record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	p := &printer{w: tw}
	p.printf("NAME\tTARGET\tPHASE\tTRIGGER\tPR\tAGE\n")
	for _, rec := range records {
		p.printf("%s\t%s\t%s\t%s\t%s\t%s\n",
			rec.Name,
			rec.Target.Name,
			phaseLabel(rec.Phase),
			rec.Trigger.Source,
			pullRequestNumber(rec.PullRequest),
			duration.HumanDuration(time.Since(rec.CreatedAt)),
		)
	}
	if p.err != nil {
		return p.err
	}
	return tw.Flush()
}

// pullRequestNumber returns the pull request number, or a dash before one exists.
func pullRequestNumber(pullRequest *v1alpha1.PullRequest) string {
	if pullRequest == nil || pullRequest.Number == 0 {
		return "-"
	}
	return strconv.Itoa(int(pullRequest.Number))
}

// phaseLabel returns the phase of a Page, or a dash before the Page has one.
func phaseLabel(phase v1alpha1.Phase) string {
	if phase == "" {
		return "-"
	}
	return string(phase)
}

// targetLabel returns a Target as kind/namespace/name.
func targetLabel(ref v1alpha1.TargetReference) string {
	if ref.Namespace == "" {
		return string(ref.Kind) + "/" + ref.Name
	}
	return string(ref.Kind) + "/" + ref.Namespace + "/" + ref.Name
}

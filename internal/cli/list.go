package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// newListCommand returns the command that lists the Triages, most recent first.
func newListCommand(conn *connection) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Triages, most recent first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := parseFormat(output)
			if err != nil {
				return err
			}
			cl, err := conn.connect()
			if err != nil {
				return err
			}
			var list v1alpha1.TriageList
			if err := cl.client.List(cmd.Context(), &list, client.InNamespace(TriageNamespace)); err != nil {
				return fmt.Errorf("list Triages: %w", err)
			}
			items := list.Items
			slices.SortFunc(items, newestFirst)
			for i := range items {
				items[i].TypeMeta = metaFor("Triage")
			}
			return writeList(cmd.OutOrStdout(), format, items)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	return cmd
}

// newestFirst orders Triages by creation time, newest first, then by name.
func newestFirst(a, b v1alpha1.Triage) int {
	switch {
	case a.CreationTimestamp.After(b.CreationTimestamp.Time):
		return -1
	case b.CreationTimestamp.After(a.CreationTimestamp.Time):
		return 1
	default:
		return strings.Compare(a.Name, b.Name)
	}
}

// writeList writes the Triages as a table, or as JSON or YAML.
func writeList(w io.Writer, format string, items []v1alpha1.Triage) error {
	if items == nil {
		items = []v1alpha1.Triage{}
	}
	list := v1alpha1.TriageList{
		TypeMeta: metaFor("TriageList"),
		Items:    items,
	}
	switch format {
	case formatJSON:
		return writeJSON(w, &list)
	case formatYAML:
		return writeYAML(w, &list)
	}
	if len(items) == 0 {
		_, err := fmt.Fprintf(w, "No Triages found in namespace %q.\n", TriageNamespace)
		return err
	}
	return writeTable(w, items)
}

// writeTable writes one row per Triage.
func writeTable(w io.Writer, items []v1alpha1.Triage) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	p := &printer{w: tw}
	p.printf("NAME\tPHASE\tWORKLOAD\tTRIGGER\tAGE\n")
	for i := range items {
		tr := &items[i]
		p.printf("%s\t%s\t%s\t%s\t%s\n",
			tr.Name,
			phaseLabel(tr.Status.Phase),
			workloadLabel(tr.Spec.Workload),
			tr.Spec.Trigger.Source,
			duration.HumanDuration(time.Since(tr.CreationTimestamp.Time)),
		)
	}
	if p.err != nil {
		return p.err
	}
	return tw.Flush()
}

// phaseLabel returns the phase of a Triage, or a dash before the Triage has one.
func phaseLabel(phase v1alpha1.Phase) string {
	if phase == "" {
		return "-"
	}
	return string(phase)
}

// workloadLabel returns a Workload as kind/namespace/name.
func workloadLabel(ref v1alpha1.WorkloadReference) string {
	return string(ref.Kind) + "/" + ref.Namespace + "/" + ref.Name
}

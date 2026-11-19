package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// newListCommand returns the command that lists the Pages, most recent first.
func newListCommand(conn *connection) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Pages, most recent first",
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
			var list v1alpha1.PageList
			if err := cl.client.List(cmd.Context(), &list, client.InNamespace(PageNamespace)); err != nil {
				return fmt.Errorf("list Pages: %w", err)
			}
			items := list.Items
			slices.SortFunc(items, newestFirst)
			for i := range items {
				items[i].TypeMeta = metaFor("Page")
			}
			return writeList(cmd.OutOrStdout(), format, items)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: json or yaml")
	return cmd
}

// newestFirst orders Pages by creation time, newest first, then by name.
func newestFirst(a, b v1alpha1.Page) int {
	switch {
	case a.CreationTimestamp.After(b.CreationTimestamp.Time):
		return -1
	case b.CreationTimestamp.After(a.CreationTimestamp.Time):
		return 1
	default:
		return strings.Compare(a.Name, b.Name)
	}
}

// writeList writes the Pages as a table, or as JSON or YAML.
func writeList(w io.Writer, format string, items []v1alpha1.Page) error {
	if items == nil {
		items = []v1alpha1.Page{}
	}
	list := v1alpha1.PageList{
		TypeMeta: metaFor("PageList"),
		Items:    items,
	}
	switch format {
	case formatJSON:
		return writeJSON(w, &list)
	case formatYAML:
		return writeYAML(w, &list)
	}
	if len(items) == 0 {
		_, err := fmt.Fprintf(w, "No Pages found in namespace %q.\n", PageNamespace)
		return err
	}
	return writeTable(w, items)
}

// writeTable writes one row per Page.
func writeTable(w io.Writer, items []v1alpha1.Page) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	p := &printer{w: tw}
	p.printf("NAME\tTARGET\tPHASE\tTRIGGER\tPR\tAGE\n")
	for i := range items {
		tr := &items[i]
		p.printf("%s\t%s\t%s\t%s\t%s\t%s\n",
			tr.Name,
			tr.Spec.Target.Name,
			phaseLabel(tr.Status.Phase),
			tr.Spec.Trigger.Source,
			pullRequestNumber(tr.Status.PullRequest),
			duration.HumanDuration(time.Since(tr.CreationTimestamp.Time)),
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
	return string(ref.Kind) + "/" + ref.Namespace + "/" + ref.Name
}

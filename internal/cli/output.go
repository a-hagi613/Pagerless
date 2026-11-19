package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Output formats that list and show accept.
const (
	formatDefault = ""
	formatJSON    = "json"
	formatYAML    = "yaml"
)

// placeholder stands for a value that is absent from a Page.
const placeholder = "none"

// metaFor returns the type metadata that machine-readable output carries for a kind of the pagerless.dev group.
func metaFor(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind}
}

// parseFormat validates the value of the --output flag.
func parseFormat(value string) (string, error) {
	switch value {
	case formatDefault, formatJSON, formatYAML:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported output format %q: use json or yaml", value)
	}
}

// writeJSON writes obj as indented JSON.
func writeJSON(w io.Writer, obj any) error {
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// writeYAML writes obj as YAML.
func writeYAML(w io.Writer, obj any) error {
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("encode YAML: %w", err)
	}
	_, err = w.Write(raw)
	return err
}

// printer writes formatted text and keeps the first error, so that a sequence
// of writes reports a failure once, at the end.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// field writes one labelled line of a presentation.
func (p *printer) field(label, value string) {
	p.printf("%-11s %s\n", label+":", value)
}

// section writes the heading of a presentation section, after a blank line.
func (p *printer) section(title string) {
	p.printf("\n%s\n", title)
}

// block writes every line of text with the indent in front of it. An empty
// text writes the placeholder.
func (p *printer) block(indent, text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		p.printf("%s%s\n", indent, placeholder)
		return
	}
	for line := range strings.SplitSeq(text, "\n") {
		p.printf("%s%s\n", indent, line)
	}
}

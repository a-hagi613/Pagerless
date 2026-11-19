// Package sandboxyaml reads and edits the YAML manifests in the Sandbox without
// reformatting them (ADR-0008). It parses a multi-document file into nodes,
// locates the Target document, and reports the source line of every mapping
// entry, so that a renderer can change only the lines of its change.
package sandboxyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// ParseDocuments returns the root node of each YAML document in the file.
func ParseDocuments(src []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(src))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the file is not valid YAML: %w", err)
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
			return nil, errors.New("the file holds an unexpected YAML document")
		}
		docs = append(docs, doc.Content[0])
	}
}

// ErrAmbiguous means that the file defines the Target more than once.
var ErrAmbiguous = errors.New("the file defines the Target more than once")

// FindTarget returns the index of the one document that defines the Target.
func FindTarget(docs []*yaml.Node, w v1alpha1.TargetReference) (int, error) {
	found := -1
	for i, doc := range docs {
		if doc.Kind != yaml.MappingNode {
			continue
		}
		if Scalar(doc, "kind") != string(w.Kind) || Scalar(Value(doc, "metadata"), "name") != w.Name {
			continue
		}
		if ns := Scalar(Value(doc, "metadata"), "namespace"); ns != "" && ns != w.Namespace {
			continue
		}
		if found >= 0 {
			return 0, fmt.Errorf("%w: %s %s", ErrAmbiguous, w.Kind, w.Name)
		}
		found = i
	}
	if found < 0 {
		return 0, fmt.Errorf("the file does not define %s %s", w.Kind, w.Name)
	}
	return found, nil
}

// Entry returns the key and value nodes of a mapping entry.
func Entry(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// Value returns the value node of a mapping entry.
func Value(m *yaml.Node, key string) *yaml.Node {
	_, v := Entry(m, key)
	return v
}

// Scalar returns the scalar value of a mapping entry, or an empty string.
func Scalar(m *yaml.Node, key string) string {
	if v := Value(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// LastLine is the last line, counting from 1, that the node or any node
// beneath it starts on.
func LastLine(n *yaml.Node) int {
	last := n.Line
	for _, c := range n.Content {
		last = max(last, LastLine(c))
	}
	return last
}

// DecodeAll returns each document decoded into maps, slices, and scalars, for a
// renderer to prove that it changed nothing but its intended field.
func DecodeAll(src []byte) ([]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(src))
	var docs []any
	for {
		var doc any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the file is not valid YAML: %w", err)
		}
		docs = append(docs, doc)
	}
}

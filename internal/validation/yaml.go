package validation

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// MaxDocumentsPerFile bounds the documents in one file.
	MaxDocumentsPerFile = 32
	// MaxNodesPerFile bounds the YAML nodes in one file.
	MaxNodesPerFile = 10000
	// MaxDepth bounds the nesting of one YAML document.
	MaxDepth = 64
)

// allowedTags are the YAML tags that a manifest may use. Anything else is an
// unknown tag, which strict parsing refuses.
var allowedTags = map[string]bool{
	"": true, "!!map": true, "!!seq": true, "!!str": true, "!!int": true,
	"!!float": true, "!!bool": true, "!!null": true, "!!timestamp": true,
}

// parseDocuments strictly parses a file's Kubernetes documents. It refuses
// duplicate keys (which the YAML decoder itself rejects), unknown tags, YAML
// aliases, documents that are not mappings, and files that exceed the document,
// node, or depth budgets.
func parseDocuments(filePath string, content []byte) ([]document, []v1alpha1.ProposalValidationError) {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	var (
		documents []document
		refusals  []v1alpha1.ProposalValidationError
	)
	refuse := func(format string, args ...any) {
		refusals = append(refusals, v1alpha1.ProposalValidationError{
			Field: "files", Message: fmt.Sprintf("%s: %s", filePath, fmt.Sprintf(format, args...)),
		})
	}
	for index := 0; ; index++ {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			refuse("is not valid YAML: %s", firstLine(err.Error()))
			return nil, refusals
		}
		if len(node.Content) == 0 {
			continue
		}
		if index >= MaxDocumentsPerFile {
			refuse("holds more than %d documents", MaxDocumentsPerFile)
			return nil, refusals
		}
		root := node.Content[0]
		if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
			continue
		}
		if root.Kind != yaml.MappingNode {
			refuse("document %d is not a mapping", index+1)
			return nil, refusals
		}
		if err := checkNode(root, 0); err != nil {
			refuse("document %d %s", index+1, err.Error())
			return nil, refusals
		}
		var value map[string]any
		if err := root.Decode(&value); err != nil {
			refuse("document %d is not a Kubernetes object: %s", index+1, firstLine(err.Error()))
			return nil, refusals
		}
		doc := document{index: index, value: value}
		doc.apiVersion, _ = value["apiVersion"].(string)
		doc.kind, _ = value["kind"].(string)
		if metadata, ok := value["metadata"].(map[string]any); ok {
			doc.name, _ = metadata["name"].(string)
			doc.namespace, _ = metadata["namespace"].(string)
		}
		if doc.apiVersion == "" || doc.kind == "" || doc.name == "" {
			refuse("document %d needs apiVersion, kind, and metadata.name", index+1)
			return nil, refusals
		}
		documents = append(documents, doc)
	}
	if len(documents) == 0 {
		refuse("holds no Kubernetes document")
	}
	return documents, refusals
}

// checkNode refuses unknown tags, aliases, excessive depth, and an excessive
// node count.
func checkNode(node *yaml.Node, depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("nests deeper than %d levels", MaxDepth)
	}
	nodes := 0
	var walk func(*yaml.Node, int) error
	walk = func(current *yaml.Node, level int) error {
		nodes++
		if nodes > MaxNodesPerFile {
			return fmt.Errorf("holds more than %d YAML nodes", MaxNodesPerFile)
		}
		if current.Alias != nil {
			return errors.New("uses a YAML alias, which a manifest must not need")
		}
		if !allowedTags[current.Tag] {
			return fmt.Errorf("uses the unknown tag %s", current.Tag)
		}
		for _, child := range current.Content {
			if err := walk(child, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(node, depth)
}

// firstLine shortens an error to its first line, so that a refusal stays inside
// the field message bound.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if runes := []rune(line); len(runes) > 300 {
		return string(runes[:300])
	}
	return line
}

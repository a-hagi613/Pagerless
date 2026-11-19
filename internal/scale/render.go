package scale

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/sandboxyaml"
)

// indentStep is the indentation of each nested level in rendered YAML.
const indentStep = 2

// errUnsupported marks manifests that the renderer declines to edit, rather than risk a wrong patch.
var errUnsupported = errors.New("unsupported manifest")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnsupported, fmt.Sprintf(format, args...))
}

// render edits the Target's replica count in the Sandbox file and returns the
// new content, changing only the replica count.
func render(src []byte, target v1alpha1.TargetReference, replicas int32) ([]byte, error) {
	if bytes.Contains(src, []byte("\r")) {
		return nil, unsupported("the file has carriage returns; use LF line endings")
	}
	docs, err := sandboxyaml.ParseDocuments(src)
	if err != nil {
		return nil, err
	}
	index, err := sandboxyaml.FindTarget(docs, target)
	if err != nil {
		return nil, err
	}
	doc := docs[index]
	specKey, specValue := sandboxyaml.Entry(doc, "spec")
	if specKey == nil || specValue == nil || specValue.Kind != yaml.MappingNode {
		return nil, unsupported("the manifest has no spec mapping")
	}

	lines := strings.SplitAfter(string(src), "\n")
	var edited []string
	key, value := sandboxyaml.Entry(specValue, "replicas")
	switch {
	case key != nil && value != nil:
		if key.Line != value.Line || value.Kind != yaml.ScalarNode {
			return nil, unsupported("replicas is not a plain scalar on one line")
		}
		line, err := replaceScalar(lines[key.Line-1], key.Column-1, value.Value, strconv.Itoa(int(replicas)))
		if err != nil {
			return nil, err
		}
		edited = slices.Clone(lines)
		edited[key.Line-1] = line
	case key == nil:
		indent := specKey.Column - 1 + indentStep
		after := sandboxyaml.LastLine(specKey)
		before := slices.Clone(lines[:after])
		if n := len(before); n > 0 && !strings.HasSuffix(before[n-1], "\n") {
			before[n-1] += "\n"
		}
		line := strings.Repeat(" ", indent) + "replicas: " + strconv.Itoa(int(replicas)) + "\n"
		edited = slices.Concat(before, []string{line}, lines[after:])
	default:
		return nil, unsupported("the manifest's spec.replicas cannot be read")
	}

	out := []byte(strings.Join(edited, ""))
	if err := verify(src, out, index, replicas); err != nil {
		return nil, err
	}
	return out, nil
}

// replaceScalar replaces the text of a mapping value on its line, keeping the
// line's indentation and any trailing comment.
func replaceScalar(line string, column int, old, replacement string) (string, error) {
	if column >= len(line) {
		return "", unsupported("the replicas entry is not on its line")
	}
	colon := strings.Index(line[column:], ":")
	if colon < 0 {
		return "", unsupported("the replicas entry has no value")
	}
	rest := column + colon + 1
	at := strings.Index(line[rest:], old)
	if at < 0 {
		return "", unsupported("the replicas value is not written as its scalar")
	}
	start := rest + at
	return line[:start] + replacement + line[start+len(old):], nil
}

// verify proves that the edit changed nothing but the Target's replica count:
// it decodes both files and compares every document, with the replica count
// held out of the comparison and checked against the requested count.
func verify(before, after []byte, index int, replicas int32) error {
	oldDocs, err := sandboxyaml.DecodeAll(before)
	if err != nil {
		return err
	}
	newDocs, err := sandboxyaml.DecodeAll(after)
	if err != nil {
		return fmt.Errorf("the rendered file is not valid YAML: %w", err)
	}
	if len(oldDocs) != len(newDocs) {
		return unsupported("the rendered file has a different number of documents")
	}
	for i := range oldDocs {
		if i == index {
			continue
		}
		if !reflect.DeepEqual(oldDocs[i], newDocs[i]) {
			return unsupported("the rendered file changed document %d", i+1)
		}
	}
	oldDoc, ok := oldDocs[index].(map[string]any)
	if !ok {
		return unsupported("the rendered file is not a mapping")
	}
	newDoc, ok := newDocs[index].(map[string]any)
	if !ok {
		return unsupported("the rendered file is not a mapping")
	}
	oldSpec, oldOK := oldDoc["spec"].(map[string]any)
	newSpec, newOK := newDoc["spec"].(map[string]any)
	if !oldOK || !newOK {
		return unsupported("the rendered file lost its spec")
	}
	if got, ok := newSpec["replicas"].(int); !ok || int32(got) != replicas {
		return unsupported("the rendered replica count differs from the requested count")
	}
	delete(oldSpec, "replicas")
	delete(newSpec, "replicas")
	if !reflect.DeepEqual(oldDoc, newDoc) {
		return unsupported("the rendering changed more than the replica count")
	}
	return nil
}

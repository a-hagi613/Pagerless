package patchresources

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/sandboxyaml"
)

// indentStep is the indentation of each nested level in rendered YAML.
const indentStep = 2

// containersPath locates a Workload's containers within its manifest.
var containersPath = []string{"spec", "template", "spec", "containers"}

// errUnsupported marks manifests that the renderer declines to edit, rather than risk a wrong patch.
var errUnsupported = errors.New("unsupported manifest")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnsupported, fmt.Sprintf(format, args...))
}

// overlay returns the resources with the requested requests and limits set.
// Entries that the request does not name keep their values.
func overlay(current corev1.ResourceRequirements, requests, limits corev1.ResourceList) corev1.ResourceRequirements {
	merged := corev1.ResourceRequirements{Claims: slices.Clone(current.Claims)}
	merged.Requests = mergeLists(current.Requests, requests)
	merged.Limits = mergeLists(current.Limits, limits)
	return merged
}

func mergeLists(current, changes corev1.ResourceList) corev1.ResourceList {
	if len(current) == 0 && len(changes) == 0 {
		return nil
	}
	merged := make(corev1.ResourceList, len(current)+len(changes))
	for name, quantity := range current {
		merged[name] = quantity.DeepCopy()
	}
	for name, quantity := range changes {
		merged[name] = quantity.DeepCopy()
	}
	return merged
}

// render edits one container's resources in the Sandbox file and returns the
// new file content, changing only the lines of the resources block. It
// returns the merged resources that the file now holds.
func render(src []byte, workload v1alpha1.WorkloadReference, container string, requests, limits corev1.ResourceList,
) ([]byte, corev1.ResourceRequirements, error) {
	var none corev1.ResourceRequirements
	if bytes.Contains(src, []byte("\r")) {
		return nil, none, unsupported("the file has carriage returns; use LF line endings")
	}
	docs, err := sandboxyaml.ParseDocuments(src)
	if err != nil {
		return nil, none, err
	}
	index, err := sandboxyaml.FindWorkload(docs, workload)
	if err != nil {
		if errors.Is(err, sandboxyaml.ErrAmbiguous) {
			return nil, none, unsupported("%v", err)
		}
		return nil, none, err
	}
	containerNode, err := findContainer(docs[index], container)
	if err != nil {
		return nil, none, err
	}
	if containerNode.Style&yaml.FlowStyle != 0 {
		return nil, none, unsupported("container %s is written in flow style; use block style", container)
	}

	resourcesKey, resourcesValue := sandboxyaml.Entry(containerNode, "resources")
	var current corev1.ResourceRequirements
	if resourcesValue != nil {
		if current, err = decodeResources(resourcesValue); err != nil {
			return nil, none, err
		}
	}
	merged := overlay(current, requests, limits)
	sections := readSections(resourcesValue)
	sections = setEntries(sections, "requests", requests)
	sections = setEntries(sections, "limits", limits)

	lines := strings.SplitAfter(string(src), "\n")
	var edited []string
	switch {
	case resourcesKey != nil:
		if resourcesKey.Line == containerNode.Line {
			return nil, none, unsupported("resources shares a line with the start of container %s", container)
		}
		if err := checkClosed(resourcesValue, lines); err != nil {
			return nil, none, err
		}
		first, last := resourcesKey.Line-1, sandboxyaml.LastLine(resourcesValue)
		edited = slices.Concat(lines[:first], blockLines(resourcesKey.Column-1, sections), lines[last:])
	default:
		anchor, err := insertionAnchor(containerNode)
		if err != nil {
			return nil, none, err
		}
		indent := containerNode.Content[0].Column - 1
		after := sandboxyaml.LastLine(anchor)
		before := slices.Clone(lines[:after])
		if n := len(before); n > 0 && !strings.HasSuffix(before[n-1], "\n") {
			before[n-1] += "\n"
		}
		edited = slices.Concat(before, blockLines(indent, sections), lines[after:])
	}
	out := []byte(strings.Join(edited, ""))
	if err := verify(src, out, index, container, merged); err != nil {
		return nil, none, err
	}
	return out, merged, nil
}

// entry is one resource in a requests or limits section, with the text that
// follows its name. Entries that a Proposal does not touch keep their text.
type entry struct {
	name, text string
}

// section is the requests or limits mapping of a resources block.
type section struct {
	name    string
	entries []entry
}

// readSections reads the existing sections in their file order.
func readSections(resources *yaml.Node) []section {
	var sections []section
	if resources == nil || resources.Kind != yaml.MappingNode {
		return sections
	}
	for i := 0; i+1 < len(resources.Content); i += 2 {
		s := section{name: resources.Content[i].Value}
		if values := resources.Content[i+1]; values.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(values.Content); j += 2 {
				s.entries = append(s.entries, entry{name: values.Content[j].Value, text: scalarText(values.Content[j+1])})
			}
		}
		sections = append(sections, s)
	}
	return sections
}

// scalarText returns a scalar as it is written, with its quotes.
func scalarText(n *yaml.Node) string {
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		return `"` + n.Value + `"`
	case n.Style&yaml.SingleQuotedStyle != 0:
		return `'` + n.Value + `'`
	default:
		return n.Value
	}
}

// setEntries sets the named resources in a section, appending the section and
// the new entries after the existing ones.
func setEntries(sections []section, name string, changes corev1.ResourceList) []section {
	if len(changes) == 0 {
		return sections
	}
	index := slices.IndexFunc(sections, func(s section) bool { return s.name == name })
	if index < 0 {
		sections = append(sections, section{name: name})
		index = len(sections) - 1
	}
	for _, resourceName := range sortedNames(changes) {
		quantity := changes[resourceName]
		e := entry{name: string(resourceName), text: quantity.String()}
		if at := slices.IndexFunc(sections[index].entries, func(x entry) bool { return x.name == e.name }); at >= 0 {
			sections[index].entries[at] = e
		} else {
			sections[index].entries = append(sections[index].entries, e)
		}
	}
	return sections
}

// blockLines renders the resources block at the indentation.
func blockLines(indent int, sections []section) []string {
	pad := strings.Repeat(" ", indent)
	lines := []string{pad + "resources:\n"}
	for _, s := range sections {
		if len(s.entries) == 0 {
			continue
		}
		lines = append(lines, pad+strings.Repeat(" ", indentStep)+s.name+":\n")
		for _, e := range s.entries {
			lines = append(lines, fmt.Sprintf("%s%s%s: %s\n", pad, strings.Repeat(" ", 2*indentStep), e.name, e.text))
		}
	}
	return lines
}

func findContainer(doc *yaml.Node, name string) (*yaml.Node, error) {
	node := doc
	for _, key := range containersPath {
		node = sandboxyaml.Value(node, key)
	}
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil, errors.New("the manifest has no spec.template.spec.containers")
	}
	for _, c := range node.Content {
		if c.Kind == yaml.MappingNode && sandboxyaml.Scalar(c, "name") == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("the manifest has no container named %s", name)
}

// insertionAnchor is the scalar node after which a new resources block goes:
// the image, or else the name, because a one-line scalar has a known last line.
func insertionAnchor(container *yaml.Node) (*yaml.Node, error) {
	for _, key := range []string{"image", "name"} {
		if _, value := sandboxyaml.Entry(container, key); value != nil && value.Kind == yaml.ScalarNode &&
			value.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
			return value, nil
		}
	}
	return nil, unsupported("the container has no plain name to anchor the resources block to")
}

// checkClosed rejects flow-style resources whose closing brace is out of reach.
func checkClosed(value *yaml.Node, lines []string) error {
	if value.Style&yaml.FlowStyle == 0 {
		return nil
	}
	last := sandboxyaml.LastLine(value)
	if last > len(lines) || !strings.ContainsAny(lines[last-1], "}") {
		return unsupported("resources is written in a multi-line flow style; use block style")
	}
	return nil
}

func decodeResources(node *yaml.Node) (corev1.ResourceRequirements, error) {
	var r corev1.ResourceRequirements
	raw, err := yaml.Marshal(node)
	if err != nil {
		return r, fmt.Errorf("read the container's resources: %w", err)
	}
	if err := sigsyaml.UnmarshalStrict(raw, &r); err != nil {
		return r, unsupported("the container's resources cannot be read: %v", err)
	}
	if len(r.Claims) > 0 {
		return r, unsupported("the container's resources use claims")
	}
	return r, nil
}

// verify proves that the edit changed nothing but the container's resources:
// it decodes both files and compares every document, with the one container's
// resources held out of the comparison and checked against the intended values.
func verify(before, after []byte, index int, container string, want corev1.ResourceRequirements) error {
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
	oldContainer, oldOK := containerMap(oldDocs[index], container)
	newContainer, newOK := containerMap(newDocs[index], container)
	if !oldOK || !newOK {
		return unsupported("the rendered file lost container %s", container)
	}
	got, err := resourcesOf(newContainer)
	if err != nil {
		return err
	}
	if !equalResources(got, want) {
		return unsupported("the rendered resources differ from the requested resources")
	}
	delete(oldContainer, "resources")
	delete(newContainer, "resources")
	if !reflect.DeepEqual(oldDocs[index], newDocs[index]) {
		return unsupported("the rendering changed more than the container's resources")
	}
	return nil
}

func containerMap(doc any, name string) (map[string]any, bool) {
	node := doc
	for _, key := range containersPath {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node = m[key]
	}
	list, ok := node.([]any)
	if !ok {
		return nil, false
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && m["name"] == name {
			return m, true
		}
	}
	return nil, false
}

func resourcesOf(container map[string]any) (corev1.ResourceRequirements, error) {
	var r corev1.ResourceRequirements
	raw, ok := container["resources"]
	if !ok {
		return r, nil
	}
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return r, fmt.Errorf("read the rendered resources: %w", err)
	}
	if err := sigsyaml.UnmarshalStrict(encoded, &r); err != nil {
		return r, unsupported("the rendered resources cannot be read: %v", err)
	}
	return r, nil
}

func equalResources(a, b corev1.ResourceRequirements) bool {
	return equalLists(a.Requests, b.Requests) && equalLists(a.Limits, b.Limits)
}

func equalLists(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for name, qa := range a {
		qb, ok := b[name]
		if !ok || qa.Cmp(qb) != 0 {
			return false
		}
	}
	return true
}

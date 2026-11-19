package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

// RepositoryValues reads a chart's values.schema.json through the base
// repository.
type RepositoryValues struct {
	// Repository reads the base commit.
	Repository Repository
}

// Schema returns the chart's values schema, or nil when the chart has none. It
// looks beside the values file first, then above it, because a chart keeps the
// schema at its root while an environment may override values in a subdirectory.
func (r RepositoryValues) Schema(ctx context.Context, commit, valuesPath string) ([]byte, error) {
	if r.Repository == nil {
		return nil, nil
	}
	dir := path.Dir(valuesPath)
	for _, candidate := range []string{
		path.Join(dir, "values.schema.json"),
		path.Join(dir, "..", "values.schema.json"),
		"values.schema.json",
	} {
		cleaned := path.Clean(candidate)
		if cleaned == "." || strings.HasPrefix(cleaned, "../") {
			continue
		}
		blob, err := r.Repository.File(ctx, commit, cleaned)
		if err != nil {
			continue
		}
		return blob.Content, nil
	}
	return nil, nil
}

// validateValuesSchema validates a Helm values document against the chart's
// JSON Schema. The schema's own rules are authoritative, so a chart that
// constrains a value refuses a Proposal that violates it.
func validateValuesSchema(schema, values []byte) ([]string, error) {
	var document any
	if err := json.Unmarshal(schema, &document); err != nil {
		return nil, fmt.Errorf("values.schema.json is not JSON: %s", firstLine(err.Error()))
	}
	compiler := jsonschema.NewCompiler()
	const location = "mem://values.schema.json"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("values.schema.json is not a JSON Schema: %s", firstLine(err.Error()))
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("values.schema.json is not a JSON Schema: %s", firstLine(err.Error()))
	}
	raw, err := yaml.YAMLToJSON(values)
	if err != nil {
		return nil, fmt.Errorf("the values are not valid YAML: %s", firstLine(err.Error()))
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("the values are not valid YAML: %s", firstLine(err.Error()))
	}
	if err := compiled.Validate(value); err != nil {
		var findings []string
		for _, line := range strings.Split(err.Error(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "jsonschema validation failed") {
				continue
			}
			findings = append(findings, "values.schema.json refuses the values: "+line)
			if len(findings) == 5 {
				break
			}
		}
		if len(findings) == 0 {
			findings = append(findings, "values.schema.json refuses the values")
		}
		return findings, nil
	}
	return nil, nil
}

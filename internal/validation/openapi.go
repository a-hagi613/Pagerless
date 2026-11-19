package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
)

const (
	// openAPIIndexPath is the API server's discovery document for its schemas.
	openAPIIndexPath = "/openapi/v3"
	// schemaResponseLimit bounds one schema document.
	schemaResponseLimit = 16 << 20
	// maxSchemaDepth bounds recursion, including $ref cycles.
	maxSchemaDepth = 100
	// maxFindings bounds the findings for one document.
	maxFindings = 10
)

// knownSchemas lists the schema name of every kind that Validation accepts. A
// name is exact because the API server publishes one schema per group and
// version, and a wrong pick would validate against the wrong object.
var knownSchemas = map[string]string{
	"apps/v1/Deployment":                     "io.k8s.api.apps.v1.Deployment",
	"apps/v1/StatefulSet":                    "io.k8s.api.apps.v1.StatefulSet",
	"apps/v1/DaemonSet":                      "io.k8s.api.apps.v1.DaemonSet",
	"batch/v1/Job":                           "io.k8s.api.batch.v1.Job",
	"batch/v1/CronJob":                       "io.k8s.api.batch.v1.CronJob",
	"v1/Pod":                                 "io.k8s.api.core.v1.Pod",
	"v1/PersistentVolumeClaim":               "io.k8s.api.core.v1.PersistentVolumeClaim",
	"v1/Service":                             "io.k8s.api.core.v1.Service",
	"v1/ConfigMap":                           "io.k8s.api.core.v1.ConfigMap",
	"networking.k8s.io/v1/Ingress":           "io.k8s.api.networking.v1.Ingress",
	"autoscaling/v2/HorizontalPodAutoscaler": "io.k8s.api.autoscaling.v2.HorizontalPodAutoscaler",
	"policy/v1/PodDisruptionBudget":          "io.k8s.api.policy.v1.PodDisruptionBudget",
}

// OpenAPI validates documents against the API server's own OpenAPI v3 schemas.
// It caches the index and one document per group and version, so a Proposal
// with several files makes one request per group and version.
type OpenAPI struct {
	// BaseURL is the API server's HTTPS address. It is required.
	BaseURL string
	// Client performs the requests. It defaults to a client with no timeout,
	// because the caller's context bounds the call.
	Client *http.Client
	// Token returns the bearer token, or an empty string for a client that the
	// transport already authenticates.
	Token func() (string, error)

	mu          sync.Mutex
	index       map[string]string
	documents   map[string]map[string]any
	unavailable map[string]bool
}

// NewOpenAPI returns an OpenAPI source for the API server at the base URL.
func NewOpenAPI(baseURL string, client *http.Client) *OpenAPI {
	return &OpenAPI{
		BaseURL: strings.TrimRight(baseURL, "/"), Client: client,
		documents: map[string]map[string]any{}, unavailable: map[string]bool{},
	}
}

// Validate validates one document and returns its structural findings.
func (o *OpenAPI) Validate(ctx context.Context, apiVersion, kind string, object map[string]any) ([]string, error) {
	document, err := o.document(ctx, groupVersionOf(apiVersion))
	if err != nil {
		return nil, err
	}
	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	name, ok := schemaName(apiVersion, kind, schemas)
	if !ok {
		return nil, fmt.Errorf("%s %s: %w", apiVersion, kind, ErrSchemaUnavailable)
	}
	schema, _ := schemas[name].(map[string]any)
	walker := &schemaWalker{documents: document, schemas: schemas}
	findings := walker.validate(object, schema, "")
	if len(findings) > maxFindings {
		findings = append(findings[:maxFindings], "and more")
	}
	return findings, nil
}

// groupVersionOf returns the OpenAPI group and version path of an apiVersion.
func groupVersionOf(apiVersion string) string {
	group, version, found := strings.Cut(apiVersion, "/")
	if !found {
		return "api/" + apiVersion
	}
	return "apis/" + group + "/" + version
}

// schemaName returns the schema name for a kind, preferring the exact known
// name and falling back to a unique suffix match for a kind that a Target
// Repository adds.
func schemaName(apiVersion, kind string, schemas map[string]any) (string, bool) {
	if name, ok := knownSchemas[apiVersion+"/"+kind]; ok {
		_, present := schemas[name]
		return name, present
	}
	_, version, found := strings.Cut(apiVersion, "/")
	if !found {
		version = apiVersion
	}
	suffix := "." + version + "." + kind
	var matches []string
	for name := range schemas {
		if strings.HasSuffix(name, suffix) {
			matches = append(matches, name)
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// index returns the index of group and version paths, reading it once.
func (o *OpenAPI) readIndex(ctx context.Context) (map[string]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.index != nil {
		return o.index, nil
	}
	var answer struct {
		Paths map[string]struct {
			ServerRelativeURL string `json:"serverRelativeURL"`
		} `json:"paths"`
	}
	if err := o.get(ctx, openAPIIndexPath, &answer); err != nil {
		return nil, fmt.Errorf("read the OpenAPI index: %w", err)
	}
	index := make(map[string]string, len(answer.Paths))
	for groupVersion, entry := range answer.Paths {
		index[groupVersion] = entry.ServerRelativeURL
	}
	o.index = index
	return o.index, nil
}

// document returns one group and version document, reading it once.
func (o *OpenAPI) document(ctx context.Context, groupVersion string) (map[string]any, error) {
	index, err := o.readIndex(ctx)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	if document, ok := o.documents[groupVersion]; ok {
		o.mu.Unlock()
		return document, nil
	}
	if o.unavailable[groupVersion] {
		o.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", groupVersion, ErrSchemaUnavailable)
	}
	relative, ok := index[groupVersion]
	o.mu.Unlock()
	if !ok {
		o.mu.Lock()
		o.unavailable[groupVersion] = true
		o.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", groupVersion, ErrSchemaUnavailable)
	}
	var document map[string]any
	apiPath := relative
	if parsed, err := url.Parse(relative); err == nil && parsed.Path != "" {
		apiPath = parsed.RequestURI()
	}
	if err := o.get(ctx, apiPath, &document); err != nil {
		return nil, fmt.Errorf("read the %s document: %w", groupVersion, err)
	}
	o.mu.Lock()
	o.documents[groupVersion] = document
	o.mu.Unlock()
	return document, nil
}

// get sends one read request and decodes its JSON answer into out.
func (o *OpenAPI) get(ctx context.Context, apiPath string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+apiPath, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if o.Token != nil {
		token, err := o.Token()
		if err != nil {
			return fmt.Errorf("read the API token: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, schemaResponseLimit))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the API server answered %d", response.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// schemaWalker validates a value against one schema document.
type schemaWalker struct {
	documents map[string]any
	schemas   map[string]any
}

// validate returns the findings for a value at a dotted path.
func (w *schemaWalker) validate(value any, schema map[string]any, path string) []string {
	return w.validateDepth(value, schema, path, 0)
}

func (w *schemaWalker) validateDepth(value any, schema map[string]any, path string, depth int) []string {
	if schema == nil {
		return nil
	}
	if depth > maxSchemaDepth {
		return []string{at(path, "the schema nests too deeply to validate")}
	}
	if reference, ok := schema["$ref"].(string); ok {
		target, ok := w.resolve(reference)
		if !ok {
			return []string{at(path, fmt.Sprintf("the schema reference %s is unknown", reference))}
		}
		return w.validateDepth(value, target, path, depth+1)
	}
	if value == nil {
		if nullable, _ := schema["nullable"].(bool); nullable {
			return nil
		}
		if _, typed := schema["type"]; typed {
			return []string{at(path, "is null, which the schema does not allow")}
		}
		return nil
	}
	var findings []string
	for _, branch := range schemaBranches(schema, "allOf") {
		findings = append(findings, w.validateDepth(value, branch, path, depth+1)...)
	}
	if branches := append(schemaBranches(schema, "anyOf"), schemaBranches(schema, "oneOf")...); len(branches) > 0 {
		var first []string
		matched := false
		for _, branch := range branches {
			found := w.validateDepth(value, branch, path, depth+1)
			if len(found) == 0 {
				matched = true
				break
			}
			if first == nil {
				first = found
			}
		}
		if !matched {
			return append(findings, first...)
		}
	}
	if intOrString, _ := schema["x-kubernetes-int-or-string"].(bool); intOrString {
		switch value.(type) {
		case string, int, int64, float64:
			return findings
		}
	}
	switch schema["type"] {
	case "object":
		findings = append(findings, w.validateObject(value, schema, path, depth)...)
	case "array":
		items, _ := value.([]any)
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range items {
			findings = append(findings, w.validateDepth(item, itemSchema,
				fmt.Sprintf("%s[%d]", path, index), depth+1)...)
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return append(findings, at(path, fmt.Sprintf("is %s, not a string", typeName(value))))
		}
		findings = append(findings, checkStringBounds(text, schema, path)...)
	case "integer":
		if _, ok := asInt(value); !ok {
			return append(findings, at(path, fmt.Sprintf("is %s, not an integer", typeName(value))))
		}
	case "number":
		switch value.(type) {
		case int, int64, float64:
		default:
			return append(findings, at(path, fmt.Sprintf("is %s, not a number", typeName(value))))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return append(findings, at(path, fmt.Sprintf("is %s, not a boolean", typeName(value))))
		}
	}
	if allowed, ok := schema["enum"].([]any); ok && len(allowed) > 0 {
		matched := slices.ContainsFunc(allowed, func(candidate any) bool {
			return reflect.DeepEqual(candidate, value)
		})
		if !matched {
			findings = append(findings, at(path, fmt.Sprintf("is %v, outside the schema's enum", value)))
		}
	}
	return findings
}

// validateObject checks an object's required fields, known properties, and
// additional properties.
func (w *schemaWalker) validateObject(value any, schema map[string]any, path string, depth int) []string {
	object, ok := value.(map[string]any)
	if !ok {
		return []string{at(path, fmt.Sprintf("is %s, not an object", typeName(value)))}
	}
	var findings []string
	if required, ok := schema["required"].([]any); ok {
		for _, entry := range required {
			name, _ := entry.(string)
			if _, present := object[name]; !present {
				findings = append(findings, at(path, fmt.Sprintf("is missing the required field %q", name)))
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	additional, hasAdditional := schema["additionalProperties"]
	preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool)
	for name, field := range object {
		fieldPath := name
		if path != "" {
			fieldPath = path + "." + name
		}
		if property, known := properties[name]; known {
			propertySchema, _ := property.(map[string]any)
			findings = append(findings, w.validateDepth(field, propertySchema, fieldPath, depth+1)...)
			continue
		}
		if preserve {
			continue
		}
		switch {
		case hasAdditional && additional == false:
			findings = append(findings, at(fieldPath, "is not a field of the schema"))
		case hasAdditional:
			if additionalSchema, ok := additional.(map[string]any); ok {
				findings = append(findings, w.validateDepth(field, additionalSchema, fieldPath, depth+1)...)
			}
		default:
			findings = append(findings, at(fieldPath,
				"is an unknown field of the schema and would be pruned by the API server"))
		}
	}
	if minimum, ok := asInt(schema["minProperties"]); ok && int64(len(object)) < minimum {
		findings = append(findings, at(path, fmt.Sprintf("has %d fields, below %d", len(object), minimum)))
	}
	return findings
}

// resolve follows a local schema reference.
func (w *schemaWalker) resolve(reference string) (map[string]any, bool) {
	const prefix = "#/components/schemas/"
	name, ok := strings.CutPrefix(reference, prefix)
	if !ok {
		return nil, false
	}
	schema, ok := w.schemas[name].(map[string]any)
	return schema, ok
}

// schemaBranches returns one composition keyword's branches.
func schemaBranches(schema map[string]any, keyword string) []map[string]any {
	list, _ := schema[keyword].([]any)
	branches := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if branch, ok := entry.(map[string]any); ok {
			branches = append(branches, branch)
		}
	}
	return branches
}

// checkStringBounds checks a string's length bounds.
func checkStringBounds(text string, schema map[string]any, path string) []string {
	var findings []string
	if minimum, ok := asInt(schema["minLength"]); ok && int64(len(text)) < minimum {
		findings = append(findings, at(path, fmt.Sprintf("is shorter than %d characters", minimum)))
	}
	if maximum, ok := asInt(schema["maxLength"]); ok && int64(len(text)) > maximum {
		findings = append(findings, at(path, fmt.Sprintf("is longer than %d characters", maximum)))
	}
	return findings
}

// at prefixes a finding with its field path.
func at(path, message string) string {
	if path == "" {
		return message
	}
	return path + " " + message
}

// typeName names a value's type for a finding.
func typeName(value any) string {
	switch value.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int, int64, float64:
		return "a number"
	default:
		return fmt.Sprintf("a %T", value)
	}
}

// ErrSchemaUnavailableFrom reports whether an error means that a schema does
// not cover an object, so Validation degrades rather than refusing.
func ErrSchemaUnavailableFrom(err error) bool { return errors.Is(err, ErrSchemaUnavailable) }

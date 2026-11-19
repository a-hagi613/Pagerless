package base

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// placeholder matches one {{ .Name }} placeholder.
var placeholder = regexp.MustCompile(`\{\{\s*\.([A-Za-z][A-Za-z0-9]*)\s*\}\}`)

// allowedPlaceholders is the closed set of template fields a Connector may
// interpolate. The server fills every one of them; a query template can never
// name a cluster, a namespace, or a tenant that the model chose.
var allowedPlaceholders = map[string]struct{}{
	"Namespace": {}, "Kind": {}, "Workload": {}, "Pod": {}, "PodPrefix": {}, "Container": {},
	"ClusterName": {}, "Environment": {}, "SinceRFC3339": {}, "UntilRFC3339": {},
}

// Template is a query template that has been validated at startup.
type Template struct {
	// name identifies the template in an error.
	name string
	// raw is the original template.
	raw string
}

// CompileTemplate validates a query template. An unknown placeholder or an
// unbalanced delimiter fails here, at startup, rather than on the first Page.
// A required template may not be empty.
func CompileTemplate(name, raw string, required bool) (*Template, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if required {
			return nil, fmt.Errorf("%s is required", name)
		}
		return &Template{name: name}, nil
	}
	if strings.Contains(trimmed, "{{") || strings.Contains(trimmed, "}}") {
		remaining := placeholder.ReplaceAllString(trimmed, "")
		if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
			return nil, fmt.Errorf("%s has an unbalanced or malformed placeholder", name)
		}
	}
	for _, match := range placeholder.FindAllStringSubmatch(trimmed, -1) {
		if _, ok := allowedPlaceholders[match[1]]; !ok {
			return nil, fmt.Errorf("%s uses the unknown placeholder {{ .%s }}", name, match[1])
		}
	}
	return &Template{name: name, raw: trimmed}, nil
}

// Raw returns the template text. It is used by tests that assert the configured
// query, never to build a request without rendering.
func (t *Template) Raw() string {
	if t == nil {
		return ""
	}
	return t.raw
}

// Render substitutes the fields. Every allowed placeholder must be supplied, so
// a template can never silently produce a query with an empty namespace.
func (t *Template) Render(fields map[string]string) (string, error) {
	if t == nil || t.raw == "" {
		return "", nil
	}
	for _, match := range placeholder.FindAllStringSubmatch(t.raw, -1) {
		if _, ok := fields[match[1]]; !ok {
			return "", fmt.Errorf("%s needs the field %s", t.name, match[1])
		}
	}
	return placeholder.ReplaceAllStringFunc(t.raw, func(match string) string {
		name := placeholder.FindStringSubmatch(match)[1]
		return fields[name]
	}), nil
}

// ValidateURL checks that a Connector URL is an absolute HTTP or HTTPS base URL
// with no embedded credentials. TLS verification is never disabled; a private
// CA is supplied through the system trust store.
func ValidateURL(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("connectors.%s.url is invalid: %w", name, err)
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return fmt.Errorf("connectors.%s.url must use http or https", name)
	case parsed.Host == "":
		return fmt.Errorf("connectors.%s.url needs a host", name)
	case parsed.User != nil:
		return fmt.Errorf("connectors.%s.url must not embed credentials; use a Secret reference", name)
	case parsed.RawQuery != "" || parsed.Fragment != "":
		return fmt.Errorf("connectors.%s.url must not carry a query or fragment", name)
	}
	return nil
}

// CompileRedaction compiles one operator redaction pattern.
func CompileRedaction(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("a redaction pattern may not be empty")
	}
	return regexp.Compile(pattern)
}

// Package contextbundle assembles, sanitizes, and stores the bounded evidence a
// Page's investigation starts from. It is deterministic: the same evidence
// always produces the same section order, the same caps, and the same hash.
package contextbundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// redacted is the marker substituted for a built-in pattern match.
const redacted = "[REDACTED]"

// builtinPattern is one value-shape redaction.
type builtinPattern struct {
	name       string
	expression *regexp.Regexp
}

// builtinPatterns are the documented value-shape redactions. They cover bearer
// tokens, JWTs, cloud access keys, Google API keys, PEM private keys, and a
// key-assignment shape that appears in logs and ConfigMaps.
var builtinPatterns = []builtinPattern{
	{"bearer", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}\b`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"pem", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"assignment", regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key)\s*[:=]\s*["']?[^\s"',}]{6,}`)},
}

// sensitiveKey matches a JSON object key whose value is secret material. The
// value is replaced even when it does not match a value-shape pattern, because
// a ConfigMap routinely stores a credential under an obvious key.
var sensitiveKey = regexp.MustCompile(
	`(?i)(password|passwd|secret|token|api[_-]?key|authorization|private[_-]?key|client[_-]?secret|` +
		`access[_-]?key|credential|session[_-]?key)`)

// Sanitizer removes secret material before a bundle is stored.
type Sanitizer struct {
	custom []builtinPattern
}

// NewSanitizer compiles the operator's own patterns. A malformed pattern is a
// configuration error, so it fails startup rather than the first Page.
func NewSanitizer(custom []string) (*Sanitizer, error) {
	compiled := make([]builtinPattern, 0, len(custom))
	for index, pattern := range custom {
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("connectors.redaction.patterns[%d] is empty", index)
		}
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("connectors.redaction.patterns[%d]: %w", index, err)
		}
		compiled = append(compiled, builtinPattern{name: fmt.Sprintf("custom-%d", index), expression: expression})
	}
	return &Sanitizer{custom: compiled}, nil
}

// Text sanitizes plain text and returns the number of replacements.
func (s *Sanitizer) Text(text string) (string, int) {
	count := 0
	for _, pattern := range builtinPatterns {
		text = replaceAll(pattern.expression, text, &count)
	}
	for _, pattern := range s.custom {
		text = replaceAll(pattern.expression, text, &count)
	}
	return text, count
}

// JSON sanitizes a JSON document: values under sensitive keys are replaced
// wholesale, then every string is passed through the value-shape patterns.
// Invalid JSON is treated as text, so a malformed backend response is still
// redacted before storage.
func (s *Sanitizer) JSON(raw []byte) ([]byte, int) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		text, count := s.Text(string(raw))
		return []byte(text), count
	}
	count := 0
	decoded = s.walk("", decoded, &count)
	encoded, err := json.Marshal(decoded)
	if err != nil {
		text, fallback := s.Text(string(raw))
		return []byte(text), count + fallback
	}
	return encoded, count
}

// walk redacts one JSON value.
func (s *Sanitizer) walk(key string, value any, count *int) any {
	if key != "" && sensitiveKey.MatchString(key) {
		if isScalar(value) {
			*count++
			return redacted
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		redactedMap := make(map[string]any, len(typed))
		for childKey, child := range typed {
			redactedMap[childKey] = s.walk(childKey, child, count)
		}
		return redactedMap
	case []any:
		items := make([]any, 0, len(typed))
		for _, child := range typed {
			items = append(items, s.walk(key, child, count))
		}
		return items
	case string:
		text, replacements := s.Text(typed)
		*count += replacements
		return text
	default:
		return value
	}
}

// isScalar reports whether a JSON value is not a container.
func isScalar(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

// replaceAll substitutes every match and counts the replacements.
func replaceAll(expression *regexp.Regexp, text string, count *int) string {
	if text == "" {
		return text
	}
	matches := expression.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	*count += len(matches)
	var builder bytes.Buffer
	last := 0
	for _, match := range matches {
		builder.WriteString(text[last:match[0]])
		builder.WriteString(redacted)
		last = match[1]
	}
	builder.WriteString(text[last:])
	return builder.String()
}

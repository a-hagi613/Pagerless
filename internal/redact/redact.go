// Package redact removes credential-shaped text from data that crosses a trust
// boundary: tool arguments, tool results, stored evidence and streamed events.
//
// It is deliberately server-owned. A model cannot disable or narrow it, and it
// runs before a result is reused, persisted or streamed, so a credential can be
// neither cited by the model nor read from the transcript later.
package redact

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Redaction classes, named in the marker so an operator can see what was
// removed without seeing it.
const (
	// Bearer is a bearer token.
	Bearer = "bearer-token"
	// JWT is a JSON Web Token.
	JWT = "jwt"
	// AWSAccessKey is an AWS access key identifier.
	AWSAccessKey = "aws-access-key"
	// PEM is a PEM private-key block.
	PEM = "pem-block"
	// GitHub is a GitHub token.
	GitHub = "github-token"
	// ProviderKey is a model or SaaS provider key.
	ProviderKey = "provider-key"
	// Credential is a credential assignment such as password = "...".
	Credential = "credential"
	// Base64 is a long base64 run, which often carries key material.
	Base64 = "base64"
	// KeyData is kubeconfig certificate or key data.
	KeyData = "key-data"
)

// pattern replaces one class of secret. Order matters: a PEM block is removed
// before its base64 body is seen, and a JWT before a bearer token.
type pattern struct {
	class      string
	expression *regexp.Regexp
}

var patterns = []pattern{
	{
		class:      PEM,
		expression: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	},
	{
		class: KeyData,
		expression: regexp.MustCompile(
			`(?m)^(\s*(?:certificate-authority-data|client-certificate-data|client-key-data)\s*:\s*).+$`),
	},
	{
		class:      JWT,
		expression: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	},
	{
		class:      GitHub,
		expression: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	},
	{
		class:      AWSAccessKey,
		expression: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[0-9A-Z]{16}\b`),
	},
	{
		class: ProviderKey,
		expression: regexp.MustCompile(
			`\b(?:sk-[A-Za-z0-9_-]{16,}|AIza[0-9A-Za-z_-]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|dop_v1_[a-f0-9]{32,})\b`),
	},
	{
		class:      Bearer,
		expression: regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`),
	},
	{
		class: Credential,
		expression: regexp.MustCompile(
			`(?i)\b(password|passwd|secret|token|api[_-]?key|client[_-]?secret|access[_-]?key|private[_-]?key)` +
				`\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;\[\]]{6,})`),
	},
	{
		class:      Base64,
		expression: regexp.MustCompile(`\b[A-Za-z0-9+/]{80,}={0,2}\b`),
	},
}

// marker returns the one marker a redacted match leaves behind.
func marker(class string) string { return "[redacted:" + class + "]" }

// Text removes every credential-shaped match from text and returns the number
// of matches removed. It leaves one marker per match, and no substring of the
// secret survives.
func Text(text string) (string, int) {
	count := 0
	for _, rule := range patterns {
		text = rule.expression.ReplaceAllStringFunc(text, func(match string) string {
			switch rule.class {
			case Bearer:
				if space := strings.IndexByte(match, ' '); space >= 0 {
					count++
					return match[:space+1] + marker(rule.class)
				}
			case KeyData, Credential:
				if separator := strings.IndexAny(match, ":="); separator >= 0 {
					count++
					return match[:separator+1] + " " + marker(rule.class)
				}
			}
			count++
			return marker(rule.class)
		})
	}
	return text, count
}

// JSON walks a decoded JSON value, redacts every string, and returns the value
// with the total number of matches removed.
func JSON(value any) (any, int) {
	count := 0
	var walk func(any) any
	walk = func(current any) any {
		switch typed := current.(type) {
		case string:
			redacted, matches := Text(typed)
			count += matches
			return redacted
		case map[string]any:
			redacted := make(map[string]any, len(typed))
			for key, item := range typed {
				redacted[key] = walk(item)
			}
			return redacted
		case []any:
			redacted := make([]any, 0, len(typed))
			for _, item := range typed {
				redacted = append(redacted, walk(item))
			}
			return redacted
		case []string:
			redacted := make([]any, 0, len(typed))
			for _, item := range typed {
				redacted = append(redacted, walk(item))
			}
			return redacted
		default:
			return value
		}
	}
	return walk(value), count
}

// Bytes decodes a JSON document, redacts every string, and returns the encoded
// document with the number of matches removed. A document that is not JSON is
// redacted as text and returned as a JSON string, so callers always receive
// valid JSON.
func Bytes(raw []byte) (json.RawMessage, int) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		text, count := Text(string(raw))
		encoded, marshalErr := json.Marshal(text)
		if marshalErr != nil {
			return json.RawMessage(`""`), count
		}
		return encoded, count
	}
	redacted, count := JSON(value)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return json.RawMessage(`null`), count
	}
	return encoded, count
}

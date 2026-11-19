package tools

import (
	"regexp"
	"strings"
)

// Redaction classes, named in the marker so an operator can see what was
// removed without seeing it.
const (
	// RedactedBearer is a bearer token.
	RedactedBearer = "bearer-token"
	// RedactedJWT is a JSON Web Token.
	RedactedJWT = "jwt"
	// RedactedAWSAccessKey is an AWS access key identifier.
	RedactedAWSAccessKey = "aws-access-key"
	// RedactedPEM is a PEM private-key block.
	RedactedPEM = "pem-block"
	// RedactedGitHub is a GitHub token.
	RedactedGitHub = "github-token"
	// RedactedProvider is a model or SaaS provider key.
	RedactedProvider = "provider-key"
	// RedactedCredential is a credential assignment such as password = "...".
	RedactedCredential = "credential"
	// RedactedBase64 is a long base64 run, which often carries key material.
	RedactedBase64 = "base64"
	// RedactedKeyData is kubeconfig certificate or key data.
	RedactedKeyData = "key-data"
)

// redactionPattern replaces one class of secret. The order matters: a PEM block
// is removed before its base64 body is seen, and a JWT before a bearer token.
type redactionPattern struct {
	class      string
	expression *regexp.Regexp
}

// redactions is the fixed server-side redaction set. It runs before the model
// sees anything and before a result is stored, so a credential can be neither
// used nor cited.
var redactions = []redactionPattern{
	{
		class:      RedactedPEM,
		expression: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	},
	{
		class: RedactedKeyData,
		expression: regexp.MustCompile(
			`(?m)^(\s*(?:certificate-authority-data|client-certificate-data|client-key-data)\s*:\s*).+$`),
	},
	{
		class:      RedactedJWT,
		expression: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	},
	{
		class:      RedactedGitHub,
		expression: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	},
	{
		class:      RedactedAWSAccessKey,
		expression: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[0-9A-Z]{16}\b`),
	},
	{
		class: RedactedProvider,
		expression: regexp.MustCompile(
			`\b(?:sk-[A-Za-z0-9_-]{16,}|AIza[0-9A-Za-z_-]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|dop_v1_[a-f0-9]{32,})\b`),
	},
	{
		class:      RedactedBearer,
		expression: regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{8,}`),
	},
	{
		class: RedactedCredential,
		expression: regexp.MustCompile(
			`(?i)\b(password|passwd|secret|token|api[_-]?key|client[_-]?secret|access[_-]?key|private[_-]?key)` +
				`\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;\[\]]{6,})`),
	},
	{
		class:      RedactedBase64,
		expression: regexp.MustCompile(`\b[A-Za-z0-9+/]{80,}={0,2}\b`),
	},
}

// marker returns the one marker a redacted match leaves behind.
func marker(class string) string {
	return "[redacted:" + class + "]"
}

// Redact removes every secret-shaped match from text and returns the number of
// matches removed. It leaves one marker per match, and no substring of the
// secret survives.
func Redact(text string) (string, int) {
	count := 0
	for _, pattern := range redactions {
		text = pattern.expression.ReplaceAllStringFunc(text, func(match string) string {
			if pattern.class == RedactedBearer {
				prefix := match[:strings.IndexByte(match, ' ')+1]
				count++
				return prefix + marker(pattern.class)
			}
			if pattern.class == RedactedKeyData {
				colon := strings.IndexAny(match, ":")
				if colon >= 0 {
					count++
					return match[:colon+1] + " " + marker(pattern.class)
				}
			}
			if pattern.class == RedactedCredential {
				separator := strings.IndexAny(match, ":=")
				if separator >= 0 {
					count++
					return match[:separator+1] + " " + marker(pattern.class)
				}
			}
			count++
			return marker(pattern.class)
		})
	}
	return text, count
}

// redactResult redacts every string in a JSON-shaped tool result and returns the
// result with the number of matches removed.
func redactResult(data any) (any, int) {
	count := 0
	var walk func(value any) any
	walk = func(value any) any {
		switch typed := value.(type) {
		case string:
			redacted, matches := Redact(typed)
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
	return walk(data), count
}

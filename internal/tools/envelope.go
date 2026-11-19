package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// EnvelopeTag is the element that delimits untrusted evidence.
const EnvelopeTag = "tool_output"

// NonceBytes is the size of the per-run nonce: 128 bits, so text written before
// a run cannot contain it.
const NonceBytes = 16

// markdownStarts are the first characters of a Markdown line that could forge
// structure around untrusted text.
const markdownStarts = "#`~>*+|-_["

// EscapeMarkdown neutralises a Markdown structure marker at the start of any
// line of untrusted text, including after leading spaces. Content that begins
// mid-line, such as a JSON string value, is unaffected.
func EscapeMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body := strings.TrimLeft(line, " ")
		if body == "" {
			continue
		}
		if strings.ContainsRune(markdownStarts, rune(body[0])) {
			indent := line[:len(line)-len(body)]
			lines[i] = indent + "\\" + body
		}
	}
	return strings.Join(lines, "\n")
}

// escapePayload escapes every string leaf of a JSON-shaped value.
func escapePayload(value any) any {
	switch typed := value.(type) {
	case string:
		return EscapeMarkdown(typed)
	case map[string]any:
		escaped := make(map[string]any, len(typed))
		for key, item := range typed {
			escaped[key] = escapePayload(item)
		}
		return escaped
	case []any:
		escaped := make([]any, 0, len(typed))
		for _, item := range typed {
			escaped = append(escaped, escapePayload(item))
		}
		return escaped
	case []string:
		escaped := make([]string, 0, len(typed))
		for _, item := range typed {
			escaped = append(escaped, EscapeMarkdown(item))
		}
		return escaped
	default:
		return value
	}
}

// EncodePayload serialises a payload as compact JSON so that a newline inside a
// log line is an escape rather than a line. Every string leaf is escaped first.
func EncodePayload(value any) ([]byte, error) {
	encoded, err := json.Marshal(escapePayload(value))
	if err != nil {
		return nil, fmt.Errorf("encode the tool payload: %w", err)
	}
	return encoded, nil
}

// Envelope wraps an untrusted payload for the model inside a nonce-delimited
// element. The document is JSON, and both tag sequences inside it are broken
// with JSON escapes, so the envelope contains exactly one real closing tag and a
// log line that contains one can only ever be a string value.
func Envelope(nonce, source string, payload any) (string, error) {
	document, err := EncodePayload(payload)
	if err != nil {
		return "", err
	}
	document = bytes.ReplaceAll(document, []byte("</"+EnvelopeTag), []byte(`<\/`+EnvelopeTag))
	document = bytes.ReplaceAll(document, []byte("<"+EnvelopeTag), []byte(`<\u0074`+EnvelopeTag))
	var out strings.Builder
	fmt.Fprintf(&out, "<%s nonce=%q source=%q>\n", EnvelopeTag, attribute(nonce), attribute(source))
	out.Write(document)
	fmt.Fprintf(&out, "\n</%s nonce=%q>", EnvelopeTag, attribute(nonce))
	return out.String(), nil
}

// attribute sanitises an attribute value so it cannot close the element early.
func attribute(value string) string {
	replacer := strings.NewReplacer(`"`, "'", "<", "(", ">", ")", "\n", " ", "\r", " ")
	value = replacer.Replace(value)
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

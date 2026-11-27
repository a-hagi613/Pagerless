package investigations

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/a-hagi613/pagerless/internal/agentclient"
	"github.com/a-hagi613/pagerless/internal/transcript"
)

// maxCitations bounds the evidence references stored with one answer.
const maxCitations = 50

// validateCitations keeps only citations that name evidence the turn actually
// recorded. A pointer must also resolve inside that evidence. A fabricated
// citation is dropped rather than published, so an answer can never cite a read
// that did not happen.
func validateCitations(citations []agentclient.Citation, evidence []transcript.Evidence) []agentclient.Citation {
	byCall := make(map[string]transcript.Evidence, len(evidence))
	for _, item := range evidence {
		byCall[item.CallID] = item
	}
	seen := make(map[string]bool, len(citations))
	valid := make([]agentclient.Citation, 0, len(citations))
	for _, citation := range citations {
		if len(valid) >= maxCitations {
			break
		}
		callID := strings.TrimSpace(citation.CallID)
		item, found := byCall[callID]
		if !found {
			continue
		}
		pointer := strings.TrimSpace(citation.Pointer)
		if pointer != "" && !resolves(item.Result, pointer) {
			continue
		}
		key := callID + "\x00" + pointer
		if seen[key] {
			continue
		}
		seen[key] = true
		valid = append(valid, agentclient.Citation{CallID: callID, Pointer: pointer})
	}
	return valid
}

// resolves reports whether an RFC 6901 pointer addresses a value in document.
func resolves(document json.RawMessage, pointer string) bool {
	if pointer == "" {
		return true
	}
	if !strings.HasPrefix(pointer, "/") {
		return false
	}
	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		return false
	}
	for raw := range strings.SplitSeq(pointer[1:], "/") {
		segment, ok := unescapePointer(raw)
		if !ok {
			return false
		}
		switch typed := value.(type) {
		case map[string]any:
			next, found := typed[segment]
			if !found {
				return false
			}
			value = next
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) || strconv.Itoa(index) != segment {
				return false
			}
			value = typed[index]
		default:
			return false
		}
	}
	return true
}

func unescapePointer(segment string) (string, bool) {
	var builder strings.Builder
	for index := 0; index < len(segment); index++ {
		if segment[index] != '~' {
			builder.WriteByte(segment[index])
			continue
		}
		if index+1 >= len(segment) {
			return "", false
		}
		switch segment[index+1] {
		case '0':
			builder.WriteByte('~')
		case '1':
			builder.WriteByte('/')
		default:
			return "", false
		}
		index++
	}
	return builder.String(), true
}

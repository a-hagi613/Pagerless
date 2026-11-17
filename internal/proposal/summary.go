package proposal

import (
	"strings"

	"github.com/a-hagi613/pagerless/internal/agentapi"
)

// maxSummaryRunes bounds the text that Summary returns, so that it fits a condition message.
const maxSummaryRunes = 1000

// Summary renders Validation errors as one line of text for a condition message.
func Summary(errs []agentapi.ValidationError) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Field+": "+e.Message)
	}
	summary := strings.Join(parts, "; ")
	if runes := []rune(summary); len(runes) > maxSummaryRunes {
		return string(runes[:maxSummaryRunes])
	}
	return summary
}

package page

import (
	"context"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/tools"
)

// maxToolCallRecords bounds the calls a Page keeps in its status.
const maxToolCallRecords = 20

// ToolSink records the tool broker's calls on the Page. It is the only writer of
// the Page's tool-call record, like every other status writer here.
type ToolSink struct {
	// Store is the only writer of Page status.
	Store *StatusStore
}

// RecordToolCall appends one call record, its redaction count, and the
// tool-call total to the Page.
func (s *ToolSink) RecordToolCall(ctx context.Context, p *v1alpha1.Page, record tools.Record) error {
	_, err := s.Store.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return nil
		}
		entry := v1alpha1.ToolCallRecord{
			CallID: record.CallID, Tool: record.Tool, Arguments: record.Arguments, Bytes: record.Bytes,
			Redactions: int32(record.Redactions), Outcome: record.Outcome, Time: metav1.NewTime(record.At),
		}
		latest.Status.ToolCalls = append(latest.Status.ToolCalls, entry)
		if excess := len(latest.Status.ToolCalls) - maxToolCallRecords; excess > 0 {
			latest.Status.ToolCalls = append([]v1alpha1.ToolCallRecord(nil), latest.Status.ToolCalls[excess:]...)
		}
		if record.Outcome == "ok" {
			latest.Status.ToolCallCount++
			latest.Status.RedactionCount += int32(record.Redactions)
		}
		return nil
	})
	return err
}

// RecordSuspiciousOutput records one SuspiciousOutput event on the Page. It also
// appends a step, so the Portal's trace shows the attempt where it happened.
func (s *ToolSink) RecordSuspiciousOutput(ctx context.Context, p *v1alpha1.Page, event string) error {
	_, err := s.Store.Update(ctx, client.ObjectKeyFromObject(p), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != v1alpha1.PhaseInvestigating {
			return nil
		}
		if containsString(latest.Status.SuspiciousOutputs, event) {
			return nil
		}
		latest.Status.SuspiciousOutputs = append(latest.Status.SuspiciousOutputs, event)
		if excess := len(latest.Status.SuspiciousOutputs) - 20; excess > 0 {
			latest.Status.SuspiciousOutputs = append([]string(nil), latest.Status.SuspiciousOutputs[excess:]...)
		}
		step := v1alpha1.Step{
			Time: metav1.NewTime(s.Store.now()), Kind: v1alpha1.StepKindSuspiciousOutput,
			Summary: truncateRunes(event, 300),
		}
		latest.Status.Steps = append(latest.Status.Steps, step)
		if excess := len(latest.Status.Steps) - v1alpha1.MaxSteps; excess > 0 {
			latest.Status.Steps = append([]v1alpha1.Step(nil), latest.Status.Steps[excess:]...)
		}
		latest.Status.StepCount++
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// containsString reports whether values holds target.
func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

// truncateRunes cuts s to at most limit runes.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

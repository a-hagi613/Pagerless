package reportapi

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/utils/clock"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/tools"
)

// The fixture's stable strings.
const (
	testCallID  = "call-1"
	testPointer = "/data/pods/0/lines/1"
	testField   = "findings[0].toolCallId"
)

// citationHandler is a Handler over one recorded tool call.
func citationHandler(t *testing.T) (*Handler, *v1alpha1.Page) {
	t.Helper()
	store := tools.NewMemoryStore()
	err := store.Record(context.Background(), tools.Record{
		PageUID: "page-uid", CallID: testCallID, Tool: "pods_logs", Outcome: "ok",
		Data: json.RawMessage(`{"pods":[{"pod":"checkout","lines":["allocating 512 MiB","Killed"]}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	page := &v1alpha1.Page{}
	page.Name, page.UID = "checkout-oom", "page-uid"
	return &Handler{Evidence: store, Clock: clock.RealClock{}}, page
}

// TestAFabricatedCitationIsRefused covers an unknown tool call and a pointer
// that does not resolve, each refused with the documented field.
func TestAFabricatedCitationIsRefused(t *testing.T) {
	t.Parallel()
	handler, page := citationHandler(t)
	cases := []struct {
		name    string
		finding v1alpha1.Finding
		field   string
	}{
		{
			name:    "an unknown tool call",
			finding: v1alpha1.Finding{Kind: v1alpha1.FindingKindLog, ToolCallID: "call-999", Pointer: testPointer},
			field:   testField,
		},
		{
			name:    "a pointer into nothing",
			finding: v1alpha1.Finding{Kind: v1alpha1.FindingKindLog, ToolCallID: testCallID, Pointer: "/data/pods/9/lines/1"},
			field:   "findings[0].pointer",
		},
		{
			name:    "a pointer into a string",
			finding: v1alpha1.Finding{Kind: v1alpha1.FindingKindLog, ToolCallID: testCallID, Pointer: "/data/pods/0/lines/1/deeper"},
			field:   "findings[0].pointer",
		},
		{
			name:    "no citation at all",
			finding: v1alpha1.Finding{Kind: v1alpha1.FindingKindLog, Pointer: testPointer},
			field:   testField,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := handler.verifyCitations(context.Background(), page, []v1alpha1.Finding{tc.finding})
			if len(errs) != 1 || errs[0].Field != tc.field {
				t.Errorf("errors = %+v, want one for %s", errs, tc.field)
			}
		})
	}
}

// TestAValidCitationIsAccepted covers a pointer into the data and one into the
// metadata the agent saw.
func TestAValidCitationIsAccepted(t *testing.T) {
	t.Parallel()
	handler, page := citationHandler(t)
	errs := handler.verifyCitations(context.Background(), page, []v1alpha1.Finding{
		{Kind: v1alpha1.FindingKindLog, ToolCallID: testCallID, Pointer: testPointer},
		{Kind: v1alpha1.FindingKindLog, ToolCallID: testCallID, Pointer: "/meta/callId"},
	})
	if len(errs) != 0 {
		t.Errorf("errors = %+v, want a valid citation accepted", errs)
	}
}

// TestACitationForAnotherPageIsRefused keeps one Page's evidence out of
// another's report.
func TestACitationForAnotherPageIsRefused(t *testing.T) {
	t.Parallel()
	handler, _ := citationHandler(t)
	other := &v1alpha1.Page{}
	other.Name, other.UID = "cart-oom", "other-page"
	errs := handler.verifyCitations(context.Background(), other, []v1alpha1.Finding{{
		Kind: v1alpha1.FindingKindLog, ToolCallID: testCallID, Pointer: testPointer,
	}})
	if len(errs) != 1 || errs[0].Field != testField {
		t.Errorf("errors = %+v, want the citation refused for another Page", errs)
	}
}

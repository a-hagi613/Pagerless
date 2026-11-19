package agentapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// rollbackReport is a report whose Proposal reverts a Sandbox commit.
func rollbackReport(commit string) Report {
	return Report{
		Diagnosis: "The commit that published a nonexistent image tag broke the Target.",
		Findings: []v1alpha1.Finding{{
			Kind:    v1alpha1.FindingKindCommit,
			Source:  "github: list recent Sandbox commits",
			Summary: "the newest commit changes the checkout image to a tag that does not exist",
		}},
		Proposal: ReportProposal{ProposedAction: v1alpha1.ProposedAction{
			Action:     v1alpha1.ActionRollback,
			Parameters: &v1alpha1.ActionParameters{Rollback: &v1alpha1.RollbackParameters{Commit: commit}},
		}},
	}
}

func TestReportAcceptsARollbackProposal(t *testing.T) {
	t.Parallel()
	if errs := rollbackReport("3f2a9c1").Check(); len(errs) != 0 {
		t.Errorf("errors = %+v, want none for a Rollback Proposal with its parameters", errs)
	}
}

func TestReportRequiresExactlyTheParametersOfItsAction(t *testing.T) {
	t.Parallel()
	const rollbackField = "proposal.parameters.rollback"
	rollback := &v1alpha1.RollbackParameters{Commit: "3f2a9c1"}
	patch := &v1alpha1.PatchResourcesParameters{Container: "app"}
	tests := []struct {
		name      string
		proposal  v1alpha1.ProposedAction
		wantField string
	}{
		{
			name:      "Rollback without parameters",
			proposal:  v1alpha1.ProposedAction{Action: v1alpha1.ActionRollback},
			wantField: rollbackField,
		},
		{
			name: "Rollback that also carries patchResources",
			proposal: v1alpha1.ProposedAction{Action: v1alpha1.ActionRollback,
				Parameters: &v1alpha1.ActionParameters{Rollback: rollback, PatchResources: patch}},
			wantField: "proposal.parameters.patchResources",
		},
		{
			name: "PatchResources that carries rollback",
			proposal: v1alpha1.ProposedAction{Action: v1alpha1.ActionPatchResources,
				Parameters: &v1alpha1.ActionParameters{PatchResources: patch, Rollback: rollback}},
			wantField: rollbackField,
		},
		{
			name: "NoAction that carries rollback",
			proposal: v1alpha1.ProposedAction{Action: v1alpha1.ActionNoAction,
				Parameters: &v1alpha1.ActionParameters{Rollback: rollback}},
			wantField: rollbackField,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := rollbackReport("3f2a9c1")
			report.Proposal = ReportProposal{ProposedAction: tc.proposal}
			errs := report.Check()
			for _, e := range errs {
				if e.Field == tc.wantField {
					return
				}
			}
			t.Errorf("errors = %+v, want one for %s", errs, tc.wantField)
		})
	}
}

func TestRollbackParametersHaveTheirWireShape(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(rollbackReport("3f2a9c1").Proposal)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"action":"Rollback","parameters":{"rollback":{"commit":"3f2a9c1"}}}`
	if string(encoded) != want {
		t.Errorf("encoded proposal = %s, want %s", encoded, want)
	}

	var decoded v1alpha1.ProposedAction
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatalf("decode the wire shape: %v", err)
	}
	if decoded.Action != v1alpha1.ActionRollback || decoded.Parameters == nil ||
		decoded.Parameters.Rollback == nil || decoded.Parameters.Rollback.Commit != "3f2a9c1" {
		t.Errorf("decoded proposal = %+v, want the Rollback Action with commit 3f2a9c1", decoded)
	}
	if strings.Contains(string(encoded), "patchResources") {
		t.Errorf("encoded proposal = %s, want only the Rollback parameters", encoded)
	}
}

package cli

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// record is one Page as both transports describe it. It is the single shape the
// presentation, the diff, and a decision read, so kubeconfig mode and server
// mode print the same fields.
type record struct {
	// Raw is the object to print for --output json or yaml: the Page resource in
	// kubeconfig mode, the Portal's record in server mode.
	Raw          any
	Name         string
	Phase        v1alpha1.Phase
	Target       v1alpha1.TargetReference
	Trigger      v1alpha1.Trigger
	TriggerCount int32
	CreatedAt    time.Time
	Hint         string
	PreviousPage string
	Failure      string
	Diagnosis    string
	Findings     []v1alpha1.Finding
	Proposal     *v1alpha1.Proposal
	Decision     *v1alpha1.Decision
	PullRequest  *v1alpha1.PullRequest
	// VerificationChecks are the post-merge checks, when Verification ran.
	VerificationChecks []v1alpha1.VerificationCheck
	Narrative          string
}

// recordFromPage renders a Page resource as the shared record.
func recordFromPage(page *v1alpha1.Page) *record {
	page.TypeMeta = metaFor("Page")
	return &record{
		Raw:                page,
		Name:               page.Name,
		Phase:              page.Status.Phase,
		Target:             page.Spec.Target,
		Trigger:            page.Spec.Trigger,
		TriggerCount:       page.Status.TriggerCount,
		CreatedAt:          page.CreationTimestamp.Time,
		Hint:               page.Spec.Hint,
		PreviousPage:       page.Spec.PreviousPage,
		Failure:            failureLabel(page),
		Diagnosis:          page.Status.Diagnosis,
		Findings:           page.Status.Findings,
		Proposal:           page.Status.Proposal,
		Decision:           page.Status.Decision,
		PullRequest:        page.Status.PullRequest,
		VerificationChecks: page.Status.VerificationChecks,
		Narrative:          page.Status.Narrative,
	}
}

// hash returns the current Proposal's hash, or an empty string.
func (r *record) hash() string {
	if r.Proposal == nil {
		return ""
	}
	return r.Proposal.Hash
}

// newestFirst orders records by creation time, newest first, then by name.
func newestFirst(a, b *record) int {
	switch {
	case a.CreatedAt.After(b.CreatedAt):
		return -1
	case b.CreatedAt.After(a.CreatedAt):
		return 1
	default:
		return strings.Compare(a.Name, b.Name)
	}
}

// failureLabel describes the reason and message of the Failed condition.
func failureLabel(page *v1alpha1.Page) string {
	condition := meta.FindStatusCondition(page.Status.Conditions, v1alpha1.ConditionFailed)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return ""
	}
	if condition.Message == "" {
		return condition.Reason
	}
	return condition.Reason + ": " + condition.Message
}

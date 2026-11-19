package page

import (
	"context"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Recorder observes the status writes that history and metrics need. It is called after a status write succeeds,
// and it never decides anything: the Page resource remains the source of truth for an Active Page.
type Recorder interface {
	// PhaseChanged records a status write. before is the status the server read, after is the status it wrote, and
	// now is the server clock's time of the write.
	PhaseChanged(ctx context.Context, before, after *v1alpha1.Page, now time.Time)
	// StepRecorded records one step event that the server accepted.
	StepRecorded(ctx context.Context, tr *v1alpha1.Page, step v1alpha1.Step, finding *v1alpha1.Finding)
}

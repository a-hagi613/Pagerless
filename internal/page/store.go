// Package page holds the helpers that every component of pagerless-server
// uses to read and write Page resources.
package page

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// StatusStore is the only path through which pagerless-server writes Page
// status (ADR-0001). Every write re-reads the Page from the API server, so a
// stale cache never causes a lost update, and retries on conflict.
type StatusStore struct {
	// Client performs the status updates.
	Client client.Client
	// Reader reads the current Page directly from the API server.
	Reader client.Reader
	// Clock stamps the terminal time. It defaults to the real clock.
	Clock clock.PassiveClock
	// Recorder observes every successful status write. It is optional.
	Recorder Recorder
}

// Update reads the Page, applies mutate to its status, and writes the status
// back if mutate changed it. If mutate returns an error, nothing is written and
// the error is returned wrapped. It returns the Page as last observed.
//
// The write also stamps TerminalAt the first time the Page reaches a terminal
// phase, so retention has one deterministic time to count from.
func (s *StatusStore) Update(
	ctx context.Context, key types.NamespacedName, mutate func(*v1alpha1.Page) error,
) (*v1alpha1.Page, error) {
	var latest v1alpha1.Page
	var before v1alpha1.Page
	var now time.Time
	var changed bool
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.Reader.Get(ctx, key, &latest); err != nil {
			return err
		}
		before = *latest.DeepCopy()
		changed = false
		if err := mutate(&latest); err != nil {
			return err
		}
		now = s.now()
		stampLifecycle(&latest.Status, now)
		if equality.Semantic.DeepEqual(before.Status, latest.Status) {
			return nil
		}
		if err := s.Client.Status().Update(ctx, &latest); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("update status of Page %s: %w", key, err)
	}
	if changed && s.Recorder != nil {
		s.Recorder.PhaseChanged(ctx, &before, &latest, now)
	}
	return &latest, nil
}

// RecordStep records one accepted step event. The status write has already happened, so a failure to record the
// step is the recorder's to log: the Page stays authoritative and the agent is not made to retry.
func (s *StatusStore) RecordStep(
	ctx context.Context, tr *v1alpha1.Page, step v1alpha1.Step, finding *v1alpha1.Finding,
) {
	if s.Recorder == nil {
		return
	}
	s.Recorder.StepRecorded(ctx, tr, step, finding)
}

// now returns the store's clock time.
func (s *StatusStore) now() time.Time {
	if s.Clock == nil {
		return time.Now()
	}
	return s.Clock.Now()
}

// stampLifecycle records the first time the server started the Page and the first time it reached a terminal
// phase. Both times come from the server's clock, so the exported time to Recovered does not depend on the API
// server's clock.
func stampLifecycle(status *v1alpha1.PageStatus, now time.Time) {
	if status.StartedAt == nil && status.Phase != "" {
		at := metav1.NewTime(now)
		status.StartedAt = &at
	}
	if status.Phase.IsTerminal() && status.TerminalAt == nil {
		at := metav1.NewTime(now)
		status.TerminalAt = &at
	}
}

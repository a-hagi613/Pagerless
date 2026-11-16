// Package triage holds the helpers that every component of pagerless-server
// uses to read and write Triage resources.
package triage

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// StatusStore is the only path through which pagerless-server writes Triage
// status (ADR-0001). Every write re-reads the Triage from the API server, so a
// stale cache never causes a lost update, and retries on conflict.
type StatusStore struct {
	// Client performs the status updates.
	Client client.Client
	// Reader reads the current Triage directly from the API server.
	Reader client.Reader
}

// Update reads the Triage, applies mutate to its status, and writes the status
// back if mutate changed it. If mutate returns an error, nothing is written and
// the error is returned wrapped. It returns the Triage as last observed.
func (s *StatusStore) Update(
	ctx context.Context, key types.NamespacedName, mutate func(*v1alpha1.Triage) error,
) (*v1alpha1.Triage, error) {
	var latest v1alpha1.Triage
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.Reader.Get(ctx, key, &latest); err != nil {
			return err
		}
		before := latest.Status.DeepCopy()
		if err := mutate(&latest); err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(*before, latest.Status) {
			return nil
		}
		return s.Client.Status().Update(ctx, &latest)
	})
	if err != nil {
		return nil, fmt.Errorf("update status of Triage %s: %w", key, err)
	}
	return &latest, nil
}

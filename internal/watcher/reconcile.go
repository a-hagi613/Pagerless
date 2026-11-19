package watcher

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
)

// reconcileTimeout bounds the startup reconciliation. A slow API server must not
// hold up the rest of the manager, because every later evaluation repairs what
// this pass could not.
const reconcileTimeout = 2 * time.Minute

// Reconcile makes the Cooldown table and the Pages agree after a restart.
//
// SQLite is authoritative for an Active Page: the Cooldown and the suppression
// counter outlive the Page resource, a deleted Page resets nothing, and a
// process that reconstructs its memory from a Page status would otherwise be
// talked into a shorter Cooldown by whatever the status happened to say. It
// therefore mirrors the row onto the Active Page, and seeds a row from the
// status only when SQLite has none, so a restored file can add state but never
// reset it.
//
// A terminal Page is never rewritten. Its Cooldown fields are history: a later
// Recurrence owns the current row, and editing a closed Page would rewrite an
// audit record that a human may already have read.
func (w *Watcher) Reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	var pages v1alpha1.PageList
	if err := w.Client.List(ctx, &pages, client.InNamespace(w.Config.Namespace)); err != nil {
		return fmt.Errorf("list Pages to reconcile the cooldown state: %w", err)
	}
	for i := range pages.Items {
		if err := w.reconcilePage(ctx, &pages.Items[i]); err != nil {
			return err
		}
	}
	logf.FromContext(ctx).V(1).Info("reconciled the cooldown state", "pages", len(pages.Items))
	return nil
}

// reconcilePage seeds a missing row from one Page, or mirrors an existing row
// onto it when the Page is still Active.
func (w *Watcher) reconcilePage(ctx context.Context, page *v1alpha1.Page) error {
	record, ok := recordFromPage(page, w.Pipeline.ClusterID)
	if !ok {
		return nil
	}
	existing, found, err := w.Store.Get(ctx, record.Fingerprint)
	if err != nil {
		return err
	}
	if !found {
		return w.Store.Seed(ctx, record)
	}
	if !page.Status.Phase.IsActive() || page.DeletionTimestamp != nil {
		return nil
	}
	return w.mirror(ctx, page, existing)
}

// mirror copies the authoritative Cooldown state onto an Active Page.
func (w *Watcher) mirror(ctx context.Context, page *v1alpha1.Page, record cooldown.Record) error {
	_, err := w.Pipeline.Status.Update(ctx, client.ObjectKeyFromObject(page), func(latest *v1alpha1.Page) error {
		if !latest.Status.Phase.IsActive() {
			return nil
		}
		lastPaged := metav1.NewTime(record.LastPagedAt)
		nextEligible := metav1.NewTime(record.CooldownUntil)
		level := max(record.NextBackoffLevel-1, 0)
		latest.Status.SuppressedCount = int32(record.SuppressedCount)
		latest.Status.Cooldown = &v1alpha1.CooldownStatus{
			LastPagedAt:     &lastPaged,
			NextEligibleAt:  &nextEligible,
			RecurrenceCount: int32(level + 1),
			BackoffLevel:    int32(level),
		}
		latest.Status.PrimaryFingerprint = record.Fingerprint
		if latest.Status.Subject == nil {
			latest.Status.Subject = &v1alpha1.SubjectStatus{
				APIVersion: record.APIVersion,
				Kind:       v1alpha1.TargetKind(record.Kind),
				Namespace:  record.Namespace,
				Name:       record.Name,
				UID:        record.UID,
			}
		}
		return nil
	})
	return err
}

// recordFromPage reconstructs a Cooldown row from a Page's status. It refuses a
// Page that carries no Fingerprint or no Subject, because a Page a human opened
// has no Cooldown to reconcile.
func recordFromPage(page *v1alpha1.Page, clusterID string) (cooldown.Record, bool) {
	fingerprint := page.Spec.Fingerprint
	if fingerprint == "" {
		fingerprint = page.Status.PrimaryFingerprint
	}
	if fingerprint == "" {
		return cooldown.Record{}, false
	}
	record := cooldown.Record{
		Fingerprint: fingerprint,
		ClusterID:   clusterID,
		ReasonClass: classOfPage(page, fingerprint),
		Scope:       scopeOfPage(page, fingerprint),
	}
	if subject := page.Status.Subject; subject != nil {
		record.APIVersion = subject.APIVersion
		record.Kind = string(subject.Kind)
		record.Namespace = subject.Namespace
		record.Name = subject.Name
		record.UID = subject.UID
		record.SubjectKey = SubjectKey(Subject{
			ClusterID:  clusterID,
			APIVersion: subject.APIVersion,
			Kind:       string(subject.Kind),
			Namespace:  subject.Namespace,
			Name:       subject.Name,
			UID:        subject.UID,
		})
	} else {
		target := page.Spec.Target
		record.APIVersion = target.APIVersion
		record.Kind = string(target.Kind)
		record.Namespace = target.Namespace
		record.Name = target.Name
		record.UID = target.UID
		record.SubjectKey = SubjectKey(Subject{
			ClusterID:  clusterID,
			APIVersion: target.APIVersion,
			Kind:       string(target.Kind),
			Namespace:  target.Namespace,
			Name:       target.Name,
			UID:        target.UID,
		})
	}
	if record.Kind == "" || record.Name == "" {
		return cooldown.Record{}, false
	}
	if window := page.Status.Cooldown; window != nil {
		record.NextBackoffLevel = int(window.BackoffLevel) + 1
		if window.LastPagedAt != nil {
			record.LastPagedAt = window.LastPagedAt.Time
		}
		if window.NextEligibleAt != nil {
			record.CooldownUntil = window.NextEligibleAt.Time
		}
	} else {
		record.NextBackoffLevel = 1
		record.LastPagedAt = page.CreationTimestamp.Time
	}
	if record.LastPagedAt.IsZero() {
		record.LastPagedAt = page.CreationTimestamp.Time
	}
	if record.CooldownUntil.IsZero() {
		record.CooldownUntil = record.LastPagedAt
	}
	record.SuppressedCount = int64(page.Status.SuppressedCount)
	record.FirstSeenAt = record.LastPagedAt
	record.LastSeenAt = record.LastPagedAt
	record.UpdatedAt = record.LastPagedAt
	return record, true
}

// classOfPage returns the reason class a Page recorded for a Fingerprint.
func classOfPage(page *v1alpha1.Page, fingerprint string) string {
	for _, entry := range page.Status.Fingerprints {
		if entry.Fingerprint == fingerprint {
			return entry.ReasonClass
		}
	}
	return ""
}

// scopeOfPage returns the scope a Page recorded for a Fingerprint.
func scopeOfPage(page *v1alpha1.Page, fingerprint string) string {
	for _, entry := range page.Status.Fingerprints {
		if entry.Fingerprint == fingerprint {
			return entry.Scope
		}
	}
	return ""
}

package watcher

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
)

// newTestWatcher builds a Watcher over the pipeline's fake API server and real
// SQLite file.
func newTestWatcher(t *testing.T, pipeline *testPipeline) *Watcher {
	t.Helper()
	return &Watcher{
		Client:   pipeline.Client,
		Store:    pipeline.Store,
		Pipeline: pipeline.Pipeline,
		Config:   Config{Namespace: testReleaseNamespace, ClusterID: pipeline.ClusterID}.withDefaults(),
	}
}

// rawPage builds a Page with the status a previous process would have written:
// it is never created by the pipeline, so its Cooldown row can be missing.
func rawPage(t *testing.T, subject Subject, fingerprint string, phase v1alpha1.Phase,
	cooldownStatus *v1alpha1.CooldownStatus, suppressed int32,
) *v1alpha1.Page {
	t.Helper()
	return &v1alpha1.Page{
		Namespace: testReleaseNamespace,
		Name:      "restored-page",
		Labels:    map[string]string{v1alpha1.LabelSubjectKey: SubjectKey(subject)},
		Spec: v1alpha1.PageSpec{
			Target:      subject.Target(),
			Fingerprint: fingerprint,
			Trigger:     v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent, Reason: testEventReason},
		},
		Status: v1alpha1.PageStatus{
			Phase:              phase,
			Subject:            subject.Status(),
			PrimaryFingerprint: fingerprint,
			Fingerprints: []v1alpha1.FingerprintEntry{{
				Fingerprint: fingerprint,
				ReasonClass: string(ReasonPodCrashLoop),
				Scope:       testScope,
				State:       v1alpha1.FingerprintPaged,
				Occurrences: 1,
			}},
			TriggerCount:    1,
			SuppressedCount: suppressed,
			Cooldown:        cooldownStatus,
		},
	}
}

// createPage stores a Page and its status through the fake API server.
func createPage(t *testing.T, pipeline *testPipeline, page *v1alpha1.Page) {
	t.Helper()
	ctx := context.Background()
	if err := pipeline.Client.Create(ctx, page); err != nil {
		t.Fatalf("create the Page: %v", err)
	}
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(page),
		func(latest *v1alpha1.Page) error {
			latest.Status = page.Status
			return nil
		}); err != nil {
		t.Fatalf("write the Page status: %v", err)
	}
}

// TestReconcileSeedsAMissingRowFromTheStatus is the restore rule: SQLite lost
// the row, the Page still carries the Cooldown, and the row is rebuilt from it
// rather than starting at the first level.
func TestReconcileSeedsAMissingRowFromTheStatus(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	watcher := newTestWatcher(t, pipeline)
	subject, _ := pipeline.deploymentSubject(t, testDeployment)
	fingerprint := FailureFingerprint(subject, ReasonPodCrashLoop, testScope)
	lastPaged := metav1.NewTime(pipelineStart)
	nextEligible := metav1.NewTime(pipelineStart.Add(2 * time.Hour))
	page := rawPage(t, subject, fingerprint, v1alpha1.PhasePending, &v1alpha1.CooldownStatus{
		LastPagedAt:     &lastPaged,
		NextEligibleAt:  &nextEligible,
		RecurrenceCount: 3,
		BackoffLevel:    2,
	}, 4)
	createPage(t, pipeline, page)

	if err := watcher.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	record, found, err := pipeline.Store.Get(context.Background(), fingerprint)
	if err != nil || !found {
		t.Fatalf("the seeded row: found=%v err=%v", found, err)
	}
	if record.NextBackoffLevel != 3 {
		t.Errorf("next backoff level = %d, want the restored level plus one", record.NextBackoffLevel)
	}
	if !record.CooldownUntil.Equal(nextEligible.Time) || record.SuppressedCount != 4 {
		t.Errorf("seeded row = %+v, want the Page's Cooldown and suppression", record)
	}
	if record.SubjectKey != SubjectKey(subject) || record.ReasonClass != string(ReasonPodCrashLoop) {
		t.Errorf("seeded row identity = %+v, want the Subject and reason class", record)
	}
}

// TestReconcileMirrorsSQLiteOntoAnActivePage is the "SQLite wins" rule: a status
// that disagrees with the row is repaired from the row.
func TestReconcileMirrorsSQLiteOntoAnActivePage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	watcher := newTestWatcher(t, pipeline)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
		[]Signal{statusSignal(reference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	created := pipeline.pages(t)[0]
	for range 5 {
		if _, err := pipeline.Store.Suppress(ctx, created.Status.PrimaryFingerprint, pipelineStart); err != nil {
			t.Fatalf("suppress: %v", err)
		}
	}
	// Corrupt the status the way a stale process or a hand edit would.
	lastPaged := metav1.NewTime(pipelineStart.Add(-time.Hour))
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(&created),
		func(latest *v1alpha1.Page) error {
			latest.Status.SuppressedCount = 0
			latest.Status.Cooldown = &v1alpha1.CooldownStatus{
				LastPagedAt:     &lastPaged,
				NextEligibleAt:  &lastPaged,
				RecurrenceCount: 1,
				BackoffLevel:    0,
			}
			return nil
		}); err != nil {
		t.Fatalf("corrupt the status: %v", err)
	}
	if err := watcher.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	record, found, err := pipeline.Store.Get(ctx, created.Status.PrimaryFingerprint)
	if err != nil || !found {
		t.Fatalf("read the row: found=%v err=%v", found, err)
	}
	var repaired v1alpha1.Page
	if err := pipeline.Client.Get(ctx, client.ObjectKeyFromObject(&created), &repaired); err != nil {
		t.Fatalf("read the repaired Page: %v", err)
	}
	if repaired.Status.SuppressedCount != int32(record.SuppressedCount) ||
		repaired.Status.SuppressedCount != 5 {
		t.Errorf("suppressed count = %d, want the row's 5", repaired.Status.SuppressedCount)
	}
	if repaired.Status.Cooldown == nil || repaired.Status.Cooldown.NextEligibleAt == nil ||
		!repaired.Status.Cooldown.NextEligibleAt.Time.Equal(record.CooldownUntil) {
		t.Errorf("cooldown = %+v, want the row's window", repaired.Status.Cooldown)
	}
	if level := repaired.Status.Cooldown.BackoffLevel; level != int32(record.NextBackoffLevel-1) {
		t.Errorf("backoff level = %d, want %d", level, record.NextBackoffLevel-1)
	}
}

// TestReconcileNeverRewritesATerminalPage keeps a closed Page an audit record:
// the current row belongs to whatever Recurrence came after it.
func TestReconcileNeverRewritesATerminalPage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	watcher := newTestWatcher(t, pipeline)
	ctx := context.Background()
	subject, _ := pipeline.deploymentSubject(t, testDeployment)
	fingerprint := FailureFingerprint(subject, ReasonPodCrashLoop, testScope)
	lastPaged := metav1.NewTime(pipelineStart)
	nextEligible := metav1.NewTime(pipelineStart.Add(15 * time.Minute))
	page := rawPage(t, subject, fingerprint, v1alpha1.PhaseRejected, &v1alpha1.CooldownStatus{
		LastPagedAt:     &lastPaged,
		NextEligibleAt:  &nextEligible,
		RecurrenceCount: 1,
		BackoffLevel:    0,
	}, 1)
	createPage(t, pipeline, page)
	if err := pipeline.Store.Seed(ctx, cooldown.Record{
		Fingerprint:      fingerprint,
		ClusterID:        pipeline.ClusterID,
		SubjectKey:       SubjectKey(subject),
		APIVersion:       subject.APIVersion,
		Kind:             subject.Kind,
		Namespace:        subject.Namespace,
		Name:             subject.Name,
		UID:              subject.UID,
		ReasonClass:      string(ReasonPodCrashLoop),
		Scope:            testScope,
		NextBackoffLevel: 4,
		LastPagedAt:      pipelineStart.Add(time.Hour),
		CooldownUntil:    pipelineStart.Add(5 * time.Hour),
		FirstSeenAt:      pipelineStart,
		LastSeenAt:       pipelineStart,
		UpdatedAt:        pipelineStart,
	}); err != nil {
		t.Fatalf("seed a later recurrence: %v", err)
	}
	if err := watcher.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var after v1alpha1.Page
	if err := pipeline.Client.Get(ctx, client.ObjectKeyFromObject(page), &after); err != nil {
		t.Fatalf("read the terminal Page: %v", err)
	}
	if after.Status.Cooldown.BackoffLevel != 0 || after.Status.SuppressedCount != 1 {
		t.Fatalf("terminal Page = %+v, want its own recorded Cooldown", after.Status.Cooldown)
	}
}

// TestReconcileIgnoresAPageWithoutAFingerprint keeps a human-opened Page out of
// the Cooldown machinery entirely.
func TestReconcileIgnoresAPageWithoutAFingerprint(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	watcher := newTestWatcher(t, pipeline)
	ctx := context.Background()
	human := &v1alpha1.Page{
		Namespace: testReleaseNamespace,
		Name:      "human-page",
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindDeployment, Namespace: testNamespace, Name: testOtherDeployment,
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceCLI},
		},
	}
	createPage(t, pipeline, human)
	if err := watcher.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if record, found, err := pipeline.Store.LastPage(ctx, pipeline.ClusterID,
		SubjectKey(Subject{})); err != nil || found {
		t.Fatalf("a Page without a Fingerprint seeded a row: %+v err=%v", record, err)
	}
}

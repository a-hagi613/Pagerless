package watcher

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
	"github.com/a-hagi613/pagerless/internal/history"
	"github.com/a-hagi613/pagerless/internal/page"
)

// pipelineStart is the fixed instant the pipeline tests start from.
var pipelineStart = time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)

// testPipeline is a Pipeline over a fake API server, a real SQLite file, a fake
// clock, and the fake owner graph the canonicalizer tests use.
type testPipeline struct {
	*Pipeline
	Cluster *fakeCluster
	Client  client.Client
	Clock   *clocktesting.FakeClock
	Store   *cooldown.Store
}

// newTestPipeline builds the production Pipeline with test doubles.
func newTestPipeline(t *testing.T, mutate func(*Config)) *testPipeline {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add the Page types to the scheme: %v", err)
	}
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Page{}).
		Build()
	fakeClock := clocktesting.NewFakeClock(pipelineStart)
	historyStore, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("open the history database: %v", err)
	}
	t.Cleanup(func() { _ = historyStore.Close() })
	store, err := cooldown.New(historyStore.DB(), cooldown.WithClock(fakeClock))
	if err != nil {
		t.Fatalf("open the cooldown store: %v", err)
	}
	cluster := newFakeCluster()
	cfg := Config{Namespace: testReleaseNamespace, ClusterID: "cluster-test", Clock: fakeClock}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg = cfg.withDefaults()
	pipeline := &Pipeline{
		Client:             reader,
		Status:             &page.StatusStore{Client: reader, Reader: reader, Clock: fakeClock},
		Resolver:           cluster.resolver(),
		Availability:       nil,
		Store:              store,
		Policy:             policyFor(cfg),
		Gate:               cfg.Gate,
		Clock:              fakeClock,
		Namespace:          cfg.Namespace,
		ClusterID:          cfg.ClusterID,
		MaxConcurrentPages: cfg.MaxConcurrentPages,
		MaxPagesPerHour:    cfg.MaxPagesPerHour,
		Observe:            cfg.Observe,
	}
	return &testPipeline{Pipeline: pipeline, Cluster: cluster, Client: reader, Clock: fakeClock, Store: store}
}

// deploymentSubject registers a Deployment and one Pod of it, and returns the
// Pod reference so a test can resolve the canonical Subject the way a signal
// does.
func (p *testPipeline) deploymentSubject(t *testing.T, name string) (Subject, Reference) {
	t.Helper()
	deployment := p.Cluster.add(testKindDeployment, testNamespace, name, "d-"+name, nil)
	replicaSet := p.Cluster.add("ReplicaSet", testNamespace, name+"-7d9f", "rs-"+name, ownerRef(deployment))
	pod := p.Cluster.add(testKindPod, testNamespace, name+"-7d9f-abc12", "p-"+name, ownerRef(replicaSet))
	subject, err := p.Resolver.Resolve(context.Background(), pod.Reference)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	// The pipeline stamps the Cluster identifier before it hashes, so a test that
	// computes a Subject Key must do the same.
	subject.ClusterID = p.ClusterID
	return subject, pod.Reference
}

// pages lists the Pages the pipeline created.
func (p *testPipeline) pages(t *testing.T) []v1alpha1.Page {
	t.Helper()
	var list v1alpha1.PageList
	if err := p.Client.List(context.Background(), &list, client.InNamespace(testReleaseNamespace)); err != nil {
		t.Fatalf("list Pages: %v", err)
	}
	return list.Items
}

// statusSignal returns a durable-status signal for a class.
func statusSignal(reference Reference, class ReasonClass, scope string) Signal {
	return Signal{
		Reference: reference,
		Class:     class,
		Scope:     scope,
		Source:    v1alpha1.TriggerSourceCondition,
		Reason:    string(class),
		FirstSeen: pipelineStart,
		Immediate: true,
	}
}

// TestTenCrashingPodsOfOneDeploymentOpenOnePage is the multiplication rule: ten
// Pods that canonicalize to one Deployment are one Subject and one Fingerprint.
func TestTenCrashingPodsOfOneDeploymentOpenOnePage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	deployment := pipeline.Cluster.add(testKindDeployment, testNamespace, testDeployment, "d-checkout", nil)
	replicaSet := pipeline.Cluster.add("ReplicaSet", testNamespace, "checkout-7d9f", "rs-checkout", ownerRef(deployment))
	for index := range 10 {
		pod := pipeline.Cluster.add(testKindPod, testNamespace, fmt.Sprintf("checkout-7d9f-%02d", index),
			fmt.Sprintf("p-%02d", index), ownerRef(replicaSet))
		subject, err := pipeline.Resolver.Resolve(ctx, pod.Reference)
		if err != nil {
			t.Fatalf("resolve Pod %d: %v", index, err)
		}
		if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
			[]Signal{statusSignal(pod.Reference, ReasonPodOOMKilled, testScope)}); err != nil {
			t.Fatalf("observe Pod %d: %v", index, err)
		}
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want exactly one for ten Pods", len(pages))
	}
	subject := Subject{
		ClusterID: "cluster-test", APIVersion: apiVersionApps, Kind: testKindDeployment, Namespace: testNamespace,
		Name: testDeployment, UID: "d-checkout",
	}
	if pages[0].Status.Subject == nil || pages[0].Status.Subject.Kind != v1alpha1.TargetKindDeployment {
		t.Fatalf("subject = %+v, want the Deployment", pages[0].Status.Subject)
	}
	if pages[0].Labels[v1alpha1.LabelSubjectKey] != SubjectKey(subject) {
		t.Fatalf("label = %q, want the Subject Key", pages[0].Labels[v1alpha1.LabelSubjectKey])
	}
	if pages[0].Status.TriggerCount != 1 {
		t.Fatalf("trigger count = %d, want 1", pages[0].Status.TriggerCount)
	}
	found, err := pipeline.activePage(ctx, SubjectKey(subject), subject.UID)
	if err != nil || found == nil {
		t.Fatalf("active page by label: found=%v err=%v, want one", found, err)
	}
}

func TestAStatusReDerivationDoesNotInflateTheTriggerCount(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	signal := statusSignal(reference, ReasonPodCrashLoop, testScope)
	signal.Immediate = true
	for index := range 3 {
		if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
			[]Signal{signal}); err != nil {
			t.Fatalf("observe status %d: %v", index, err)
		}
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 || pages[0].Status.TriggerCount != 1 {
		t.Fatalf("pages = %d, trigger count = %d, want one Page and one Trigger", len(pages),
			pages[0].Status.TriggerCount)
	}
}

// TestASecondReasonJoinsTheActivePage is the join rule: the count, the time, and
// the distinct Fingerprint move, and no second Page exists.
func TestASecondReasonJoinsTheActivePage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	first := statusSignal(reference, ReasonPodOOMKilled, testScope)
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2}, []Signal{first}); err != nil {
		t.Fatalf("observe the first reason: %v", err)
	}
	pipeline.Clock.Step(time.Minute)
	second := statusSignal(reference, ReasonStorageFailedMount, "data")
	second.FirstSeen = pipelineStart.Add(time.Minute)
	if err := pipeline.HandleEvent(ctx, second); err != nil {
		t.Fatalf("ingest the second reason: %v", err)
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want the second reason to join", len(pages))
	}
	state := pages[0].Status
	if state.TriggerCount != 2 {
		t.Fatalf("trigger count = %d, want 2", state.TriggerCount)
	}
	if state.LastTriggerAt == nil || !state.LastTriggerAt.Time.Equal(pipelineStart.Add(time.Minute)) {
		t.Fatalf("last trigger at = %v, want the second Trigger", state.LastTriggerAt)
	}
	if len(state.Fingerprints) != 2 {
		t.Fatalf("fingerprints = %d, want both reasons recorded", len(state.Fingerprints))
	}
	if state.Severity != v1alpha1.SeverityWarning {
		t.Fatalf("severity = %s, want warning from the second reason", state.Severity)
	}
}

// TestARepeatInsideTheAggregationWindowCountsWithoutANewTrigger keeps a restart
// storm to one Trigger and one Page.
func TestARepeatInsideTheAggregationWindowCountsWithoutANewTrigger(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	first := statusSignal(reference, ReasonPodCrashLoop, testScope)
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2}, []Signal{first}); err != nil {
		t.Fatalf("observe the first Trigger: %v", err)
	}
	pipeline.Clock.Step(time.Minute)
	repeat := first
	repeat.Source = v1alpha1.TriggerSourceEvent
	repeat.Count = 400
	repeat.FirstSeen = pipelineStart.Add(time.Minute)
	if err := pipeline.HandleEvent(ctx, repeat); err != nil {
		t.Fatalf("ingest the repeat: %v", err)
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 || pages[0].Status.TriggerCount != 1 {
		t.Fatalf("trigger count = %d, want 1 inside the aggregation window", pages[0].Status.TriggerCount)
	}
	observation, found, err := pipeline.Store.Observation(ctx, pages[0].Status.PrimaryFingerprint)
	if err != nil || !found {
		t.Fatalf("read the observation: found=%v err=%v", found, err)
	}
	if observation.Occurrences != 401 {
		t.Fatalf("occurrences = %d, want the first observation plus the Event count 400", observation.Occurrences)
	}
	// After the window the same failure is a new Trigger, and it joins rather
	// than opening a second Page.
	pipeline.Clock.Step(DefaultAggregationWindow)
	late := repeat
	late.FirstSeen = pipelineStart.Add(DefaultAggregationWindow + 2*time.Minute)
	late.Count = 401
	if err := pipeline.HandleEvent(ctx, late); err != nil {
		t.Fatalf("ingest the later Trigger: %v", err)
	}
	pages = pipeline.pages(t)
	if len(pages) != 1 || pages[0].Status.TriggerCount != 2 {
		t.Fatalf("pages = %d, trigger count = %d, want a join", len(pages), pages[0].Status.TriggerCount)
	}
}

// TestATriggerInsideTheCooldownOpensNothingAndEscalatesTheNextPage is the budget
// rule end to end: repeated Triggers inside a Cooldown are counted, and the Page
// that eventually opens carries the count and the escalated severity.
func TestATriggerInsideTheCooldownOpensNothingAndEscalatesTheNextPage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	now := pipelineStart
	fingerprint := FailureFingerprint(subject, ReasonPodOOMKilled, testScope)
	window, err := pipeline.Store.BeginPage(ctx, cooldown.Record{
		Fingerprint: fingerprint,
		ClusterID:   pipeline.ClusterID,
		SubjectKey:  SubjectKey(subject),
		APIVersion:  subject.APIVersion,
		Kind:        subject.Kind,
		Namespace:   subject.Namespace,
		Name:        subject.Name,
		UID:         subject.UID,
		ReasonClass: string(ReasonPodOOMKilled),
		Scope:       testScope,
		FirstSeenAt: now,
	}, now)
	if err != nil {
		t.Fatalf("open the previous cooldown window: %v", err)
	}

	signal := statusSignal(reference, ReasonPodOOMKilled, testScope)
	for index := range 10 {
		pipeline.Clock.Step(time.Second)
		signal.FirstSeen = pipeline.Clock.Now()
		if err := pipeline.HandleEvent(ctx, signal); err != nil {
			t.Fatalf("ingest suppressed Trigger %d: %v", index, err)
		}
	}
	if pages := pipeline.pages(t); len(pages) != 0 {
		t.Fatalf("pages = %d inside the Cooldown, want none", len(pages))
	}
	stored, found, err := pipeline.Store.Get(ctx, fingerprint)
	if err != nil || !found {
		t.Fatalf("read the suppressed row: found=%v err=%v", found, err)
	}
	if stored.SuppressedCount != 10 {
		t.Fatalf("suppressed count = %d, want 10", stored.SuppressedCount)
	}

	pipeline.Clock.Step(window.CooldownUntil.Sub(pipeline.Clock.Now()) + time.Second)
	signal.FirstSeen = pipeline.Clock.Now()
	if err := pipeline.HandleEvent(ctx, signal); err != nil {
		t.Fatalf("ingest the Trigger after the Cooldown: %v", err)
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 {
		t.Fatalf("pages = %d after the Cooldown, want one", len(pages))
	}
	state := pages[0].Status
	if state.SuppressedCount != 10 {
		t.Fatalf("Page suppressed count = %d, want the accumulated 10", state.SuppressedCount)
	}
	if state.Severity != v1alpha1.SeverityWarning {
		t.Fatalf("severity = %s, want the suppression-escalated warning", state.Severity)
	}
	if state.Cooldown == nil || state.Cooldown.BackoffLevel != 1 {
		t.Fatalf("cooldown = %+v, want the escalated level 1", state.Cooldown)
	}
}

func TestTheBurstCeilingSuppressesASingleNoisySubject(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	_, reference := pipeline.deploymentSubject(t, testDeployment)
	signal := statusSignal(reference, ReasonPodCrashLoop, testScope)
	signal.Count = DefaultBurst + 1
	if err := pipeline.HandleEvent(ctx, signal); err != nil {
		t.Fatalf("ingest the burst: %v", err)
	}
	if pages := pipeline.pages(t); len(pages) != 0 {
		t.Fatalf("pages = %d past the burst ceiling, want none", len(pages))
	}
}

func TestAnIgnoredTargetOpensNoPage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	subject.Annotations = map[string]string{v1alpha1.AnnotationIgnore: "true"}
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
		[]Signal{statusSignal(reference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe an ignored Target: %v", err)
	}
	if pages := pipeline.pages(t); len(pages) != 0 {
		t.Fatalf("pages = %d on an ignored Target, want none", len(pages))
	}
}

// TestWarnOnlyOpensAVisiblePageWithoutAnInvestigation is the kube-system rule.
func TestWarnOnlyOpensAVisiblePageWithoutAnInvestigation(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	deployment := pipeline.Cluster.add(testKindDeployment, kubeSystemNamespace, "coredns", testDeploymentUID, nil)
	subject, err := pipeline.Resolver.Resolve(ctx, deployment.Reference)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	signal := Signal{
		Reference: deployment.Reference,
		Class:     ReasonPodOOMKilled,
		Source:    v1alpha1.TriggerSourceCondition,
		FirstSeen: pipelineStart,
		Immediate: true,
	}
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 1, Ready: 0}, []Signal{signal}); err != nil {
		t.Fatalf("observe kube-system: %v", err)
	}
	pages := pipeline.pages(t)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want the visible warn-only Page", len(pages))
	}
	investigation := pages[0].Status.Investigation
	if investigation == nil || investigation.State != v1alpha1.InvestigationSuppressed ||
		investigation.Reason != v1alpha1.ReasonWarnOnly {
		t.Fatalf("investigation = %+v, want a suppressed WarnOnly investigation", investigation)
	}
}

// TestTheConcurrencyCeilingQueuesTheRest proves a Page that waits for a slot is
// visible with its reason rather than dropped.
func TestTheConcurrencyCeilingQueuesTheRest(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, func(cfg *Config) { cfg.MaxConcurrentPages = 1 })
	ctx := context.Background()
	firstSubject, firstReference := pipeline.deploymentSubject(t, testDeployment)
	if err := pipeline.HandleStatus(ctx, firstSubject, Availability{Desired: 1, Ready: 0},
		[]Signal{statusSignal(firstReference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe the first Subject: %v", err)
	}
	first := pipeline.pages(t)
	if len(first) != 1 {
		t.Fatalf("pages = %d, want one", len(first))
	}
	// The Page controller moves the first Page to Investigating and starts its Job.
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(&first[0]),
		func(latest *v1alpha1.Page) error {
			latest.Status.Phase = v1alpha1.PhaseInvestigating
			return nil
		}); err != nil {
		t.Fatalf("start the first investigation: %v", err)
	}
	secondSubject, secondReference := pipeline.deploymentSubject(t, testOtherDeployment)
	if err := pipeline.HandleStatus(ctx, secondSubject, Availability{Desired: 1, Ready: 0},
		[]Signal{statusSignal(secondReference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe the second Subject: %v", err)
	}
	for _, candidate := range pipeline.pages(t) {
		if candidate.Status.Subject == nil || candidate.Status.Subject.Name != testOtherDeployment {
			continue
		}
		investigation := candidate.Status.Investigation
		if investigation == nil || investigation.State != v1alpha1.InvestigationQueued ||
			investigation.Reason != v1alpha1.ReasonConcurrencyLimit {
			t.Fatalf("investigation = %+v, want a queued investigation", investigation)
		}
		return
	}
	t.Fatal("the second Subject did not open a Page")
}

// TestPromoteQueuedGivesASlotBackToTheOldestQueuedPage proves the queue drains
// rather than leaking Pages that never start.
func TestPromoteQueuedGivesASlotBackToTheOldestQueuedPage(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, func(cfg *Config) { cfg.MaxConcurrentPages = 1 })
	ctx := context.Background()
	firstSubject, firstReference := pipeline.deploymentSubject(t, testDeployment)
	if err := pipeline.HandleStatus(ctx, firstSubject, Availability{Desired: 1, Ready: 0},
		[]Signal{statusSignal(firstReference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe the first Subject: %v", err)
	}
	running := pipeline.pages(t)[0]
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(&running),
		func(latest *v1alpha1.Page) error {
			latest.Status.Phase = v1alpha1.PhaseInvestigating
			return nil
		}); err != nil {
		t.Fatalf("start the first investigation: %v", err)
	}
	secondSubject, secondReference := pipeline.deploymentSubject(t, testOtherDeployment)
	if err := pipeline.HandleStatus(ctx, secondSubject, Availability{Desired: 1, Ready: 0},
		[]Signal{statusSignal(secondReference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe the second Subject: %v", err)
	}
	// Free the slot the way the Page controller does when the investigation ends.
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(&running),
		func(latest *v1alpha1.Page) error {
			latest.Status.Phase = v1alpha1.PhasePending
			return nil
		}); err != nil {
		t.Fatalf("free the slot: %v", err)
	}
	promoted, err := pipeline.PromoteQueued(ctx)
	if err != nil {
		t.Fatalf("promote the queue: %v", err)
	}
	if promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
	}
	for _, candidate := range pipeline.pages(t) {
		if candidate.Status.Investigation != nil &&
			candidate.Status.Investigation.State == v1alpha1.InvestigationQueued {
			t.Fatalf("Page %s is still queued after a slot was freed", candidate.Name)
		}
	}
}

func TestMarkFingerprintStateAndCloseClearedPages(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
		[]Signal{statusSignal(reference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	created := pipeline.pages(t)[0]
	if err := pipeline.MarkFingerprintState(ctx, SubjectKey(subject), created.Status.PrimaryFingerprint,
		v1alpha1.FingerprintResolved); err != nil {
		t.Fatalf("mark the Fingerprint resolved: %v", err)
	}
	if err := pipeline.CloseClearedPages(ctx, SubjectKey(subject), pipelineStart.Add(time.Hour)); err != nil {
		t.Fatalf("close the cleared Page: %v", err)
	}
	var closed v1alpha1.Page
	if err := pipeline.Client.Get(ctx, client.ObjectKeyFromObject(&created), &closed); err != nil {
		t.Fatalf("read the closed Page: %v", err)
	}
	if closed.Status.Phase != v1alpha1.PhaseRecovered || closed.Status.TerminalAt == nil {
		t.Fatalf("phase = %s terminalAt = %v, want a closed Recovered Page", closed.Status.Phase,
			closed.Status.TerminalAt)
	}
	if len(closed.Status.Fingerprints) != 1 ||
		closed.Status.Fingerprints[0].State != v1alpha1.FingerprintResolved {
		t.Fatalf("fingerprints = %+v, want the resolved state", closed.Status.Fingerprints)
	}
}

// TestCloseClearedPagesNeverAbandonsADecision keeps a human's approval or a pull
// request intact when the failure happens to clear.
func TestCloseClearedPagesNeverAbandonsADecision(t *testing.T) {
	t.Parallel()
	pipeline := newTestPipeline(t, nil)
	ctx := context.Background()
	subject, reference := pipeline.deploymentSubject(t, testDeployment)
	if err := pipeline.HandleStatus(ctx, subject, Availability{Desired: 3, Ready: 2},
		[]Signal{statusSignal(reference, ReasonPodOOMKilled, testScope)}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	created := pipeline.pages(t)[0]
	if _, err := pipeline.Status.Update(ctx, client.ObjectKeyFromObject(&created),
		func(latest *v1alpha1.Page) error {
			latest.Status.Phase = v1alpha1.PhaseApproved
			return nil
		}); err != nil {
		t.Fatalf("approve the Page: %v", err)
	}
	if err := pipeline.CloseClearedPages(ctx, SubjectKey(subject), pipelineStart.Add(time.Hour)); err != nil {
		t.Fatalf("close cleared Pages: %v", err)
	}
	var after v1alpha1.Page
	if err := pipeline.Client.Get(ctx, client.ObjectKeyFromObject(&created), &after); err != nil {
		t.Fatalf("read the Page: %v", err)
	}
	if after.Status.Phase != v1alpha1.PhaseApproved {
		t.Fatalf("phase = %s, want the approved Page untouched", after.Status.Phase)
	}
}

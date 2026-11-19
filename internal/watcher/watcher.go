package watcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
	"github.com/a-hagi613/pagerless/internal/page"
)

// Defaults for the Watcher. They are the values the v2 specification documents,
// so an installation that sets nothing still behaves as specified.
const (
	// DefaultClearFor is how long a failure must be absent from live status before
	// its observations clear and its Page closes.
	DefaultClearFor = 5 * time.Minute
	// DefaultStatusRequeue is how often an observed object is re-derived.
	DefaultStatusRequeue = time.Minute
	// DefaultEvaluationInterval is how often cleared observations, healthy resets,
	// and the queue are evaluated.
	DefaultEvaluationInterval = time.Minute
	// DefaultMaxConcurrentPages bounds running investigations.
	DefaultMaxConcurrentPages = 3
	// DefaultMaxPagesPerHour bounds investigations started per sliding hour.
	DefaultMaxPagesPerHour = 12
)

// Config configures the watcher.
type Config struct {
	// Namespace is the release namespace: it holds Pages and is never watched.
	Namespace string
	// ClusterID identifies the Cluster. An empty value is resolved from the
	// kube-system namespace UID, then from the release namespace UID.
	ClusterID string
	// IncludeNamespaces restricts watching when it is non-empty.
	IncludeNamespaces []string
	// ExcludeNamespaces drops every listed namespace.
	ExcludeNamespaces []string
	// KubeSystemMode is how kube-system is treated. It defaults to warn-only.
	KubeSystemMode NamespaceMode
	// NodePagesMode is how Node faults are treated. It defaults to watch.
	NodePagesMode NamespaceMode
	// MinSeverity is the lowest severity that opens a Page. It defaults to warning.
	MinSeverity v1alpha1.Severity
	// AggregationWindow is the window inside which repeats count as occurrences.
	AggregationWindow time.Duration
	// Burst is the per-Subject occurrence ceiling inside one window.
	Burst int64
	// MaxConcurrentPages bounds running investigations.
	MaxConcurrentPages int
	// MaxPagesPerHour bounds investigations started per sliding hour.
	MaxPagesPerHour int
	// ClearFor is how long a failure must be absent before it clears.
	ClearFor time.Duration
	// StatusRequeue is how often an observed object is re-derived.
	StatusRequeue time.Duration
	// EvaluationInterval is how often cleared observations and the queue are evaluated.
	EvaluationInterval time.Duration
	// Ladder is the Cooldown ladder. It defaults to the specified one.
	Ladder cooldown.Ladder
	// Observe tunes the status thresholds.
	Observe ObserveConfig
	// Gate overrides the gate. Zero fields fall back to the specified defaults.
	Gate Gate
	// Clock is the clock the watcher reads. It defaults to the real clock.
	Clock clock.PassiveClock
}

// withDefaults fills every unset field.
func (c Config) withDefaults() Config {
	if c.KubeSystemMode == "" {
		c.KubeSystemMode = ModeWarnOnly
	}
	if c.NodePagesMode == "" {
		c.NodePagesMode = ModeWatch
	}
	if c.MinSeverity == "" {
		c.MinSeverity = v1alpha1.SeverityWarning
	}
	if c.AggregationWindow <= 0 {
		c.AggregationWindow = DefaultAggregationWindow
	}
	if c.Burst <= 0 {
		c.Burst = DefaultBurst
	}
	if c.MaxConcurrentPages <= 0 {
		c.MaxConcurrentPages = DefaultMaxConcurrentPages
	}
	if c.MaxPagesPerHour <= 0 {
		c.MaxPagesPerHour = DefaultMaxPagesPerHour
	}
	if c.ClearFor <= 0 {
		c.ClearFor = DefaultClearFor
	}
	if c.StatusRequeue <= 0 {
		c.StatusRequeue = DefaultStatusRequeue
	}
	if c.EvaluationInterval <= 0 {
		c.EvaluationInterval = DefaultEvaluationInterval
	}
	if len(c.Ladder.Steps) == 0 {
		c.Ladder = cooldown.DefaultLadder()
	}
	if c.Clock == nil {
		c.Clock = clock.RealClock{}
	}
	return c
}

// Watcher runs the dual-API event ingestor, the status observers, and the
// evaluation loop that clears failures, resets healthy Cooldowns, and starts
// queued investigations.
type Watcher struct {
	// Client reads and writes Pages.
	Client client.Client
	// Store is the durable Cooldown and observation state.
	Store *cooldown.Store
	// Pipeline decides what each signal opens, joins, or suppresses.
	Pipeline *Pipeline
	// Config is the resolved configuration.
	Config Config
}

// New builds a Watcher over the history database handle. The caller registers
// it with Setup on the same manager that owns the cache.
func New(mgr ctrl.Manager, db *sql.DB, cfg Config) (*Watcher, error) {
	cfg = cfg.withDefaults()
	if cfg.Namespace == "" {
		return nil, errors.New("the watcher needs the release namespace")
	}
	lookup := &ClusterLookup{Reader: mgr.GetClient()}
	resolver := &Resolver{Lookup: lookup, Claims: lookup}
	store, err := cooldown.New(db, cooldown.WithLadder(cfg.Ladder), cooldown.WithClock(cfg.Clock))
	if err != nil {
		return nil, fmt.Errorf("open the cooldown store: %w", err)
	}
	clusterID := cfg.ClusterID
	if clusterID == "" {
		resolved, err := ResolveClusterID(context.Background(), mgr.GetAPIReader(), cfg.Namespace)
		if err != nil {
			return nil, err
		}
		clusterID = resolved
	}
	pipeline := &Pipeline{
		Client:             mgr.GetClient(),
		Status:             nil,
		Resolver:           resolver,
		Availability:       lookup,
		Store:              store,
		Policy:             policyFor(cfg),
		Gate:               cfg.Gate,
		Clock:              cfg.Clock,
		Namespace:          cfg.Namespace,
		ClusterID:          clusterID,
		MaxConcurrentPages: cfg.MaxConcurrentPages,
		MaxPagesPerHour:    cfg.MaxPagesPerHour,
		Observe:            cfg.Observe,
	}
	return &Watcher{Client: mgr.GetClient(), Store: store, Pipeline: pipeline, Config: cfg}, nil
}

// policyFor builds the namespace policy from the configuration.
func policyFor(cfg Config) Policy {
	return Policy{
		ReleaseNamespace: cfg.Namespace,
		Include:          cfg.IncludeNamespaces,
		Exclude:          cfg.ExcludeNamespaces,
		KubeSystem:       cfg.KubeSystemMode,
		NodePages:        cfg.NodePagesMode,
	}
}

// Setup registers the event ingesters, the status observers, and the evaluation
// loop with the manager. The status store must be set on the Pipeline first,
// because the Pipeline is the only writer of Page status.
func (w *Watcher) Setup(mgr ctrl.Manager, status *page.StatusStore) error {
	w.Pipeline.Status = status
	for _, source := range []EventSource{SourceCoreV1, SourceEventsV1} {
		ingester := &EventReconciler{Client: mgr.GetClient(), Pipeline: w.Pipeline, Source: source}
		if err := ingester.setup(mgr); err != nil {
			return fmt.Errorf("set up the %s event ingester: %w", source, err)
		}
	}
	for _, kind := range observableKinds {
		if kind == v1alpha1.TargetKindNode && w.Config.NodePagesMode == ModeIgnore {
			// Mode B has no cluster-scoped read rules, so a Node observer could
			// never sync its informer and would stop the manager from starting.
			continue
		}
		observer := &StatusReconciler{
			Client:       mgr.GetClient(),
			Resolver:     w.Pipeline.Resolver,
			Pipeline:     w.Pipeline,
			Kind:         kind,
			Observe:      w.Config.Observe,
			RequeueAfter: w.Config.StatusRequeue,
		}
		if err := observer.setup(mgr); err != nil {
			return fmt.Errorf("set up the %s observer: %w", kind, err)
		}
	}
	return mgr.Add(w)
}

// Start reconciles the durable Cooldown state once, then runs the evaluation
// loop until the manager stops.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.Reconcile(ctx); err != nil && ctx.Err() == nil {
		logf.FromContext(ctx).Error(err, "reconcile the cooldown state on startup")
	}
	ticker := time.NewTicker(w.Config.EvaluationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.Evaluate(ctx); err != nil && ctx.Err() == nil {
				logf.FromContext(ctx).Error(err, "evaluate the watcher")
			}
		}
	}
}

// Evaluate clears observations nothing re-derived, resets Cooldowns whose
// healthy window elapsed, and starts queued investigations while slots remain.
func (w *Watcher) Evaluate(ctx context.Context) error {
	now := w.Pipeline.now()
	stale, err := w.Store.StaleObservations(ctx, w.Config.ClearFor, now)
	if err != nil {
		return err
	}
	subjects := make(map[string]struct{}, len(stale))
	for _, observation := range stale {
		if err := w.Store.ResolveObservation(ctx, observation.Fingerprint, now); err != nil {
			return err
		}
		if err := w.Pipeline.MarkFingerprintState(ctx, observation.SubjectKey, observation.Fingerprint,
			v1alpha1.FingerprintResolved); err != nil {
			return err
		}
		subjects[observation.SubjectKey] = struct{}{}
	}
	for subjectKey := range subjects {
		active, err := w.Store.ActiveObservations(ctx, subjectKey)
		if err != nil {
			return err
		}
		if active {
			continue
		}
		if err := w.Store.MarkResolved(ctx, subjectKey, now); err != nil {
			return err
		}
		if err := w.Pipeline.CloseClearedPages(ctx, subjectKey, now); err != nil {
			return err
		}
	}
	if _, err := w.Store.Reset(ctx, now); err != nil {
		return err
	}
	_, err = w.Pipeline.PromoteQueued(ctx)
	return err
}

// ResolveClusterID returns the Cluster identifier the watcher hashes into every
// Fingerprint. It reads the kube-system namespace UID, which survives control
// plane restarts and needs no stored object, and falls back to the release
// namespace's UID. An installation whose read Role is namespaced (ADR-0038)
// cannot read either namespace object, so the last resort is the release
// namespace name: it is stable across restarts, and one SQLite file belongs to
// one installation, so it cannot collide with another Cluster's Cooldowns. It
// never invents a random identifier, because a restart would then change every
// Fingerprint and reset every Cooldown.
func ResolveClusterID(ctx context.Context, reader client.Reader, releaseNamespace string) (string, error) {
	for _, name := range []string{kubeSystemNamespace, releaseNamespace} {
		if name == "" {
			continue
		}
		var namespace corev1.Namespace
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
			continue
		}
		if namespace.UID != "" {
			return string(namespace.UID), nil
		}
	}
	if releaseNamespace != "" {
		return "namespace/" + releaseNamespace, nil
	}
	return "", errors.New("cannot resolve a cluster identifier: neither kube-system nor the release namespace is readable")
}

// CacheByObject returns the informer configuration the watcher needs: every
// observed kind cached across the namespaces the installation may read, because
// the manager's default namespace scope is the release namespace and a watcher
// that only saw its own namespace would see nothing worth paging on.
//
// The include list is the Mode B allowlist. A namespaced Role cannot list a kind
// across the cluster, so the cache must be restricted to the same namespaces the
// RBAC role grants, or the informer never syncs. Nodes are dropped with the
// cluster-scoped rules that reading them needs.
func CacheByObject(include []string, nodePages NamespaceMode) map[client.Object]cache.ByObject {
	namespaces := map[string]cache.Config{metav1.NamespaceAll: {}}
	if len(include) > 0 {
		namespaces = make(map[string]cache.Config, len(include))
		for _, namespace := range include {
			namespaces[namespace] = cache.Config{}
		}
	}
	observed := map[client.Object]cache.ByObject{
		&corev1.Event{}:                 {Namespaces: namespaces},
		&eventsv1.Event{}:               {Namespaces: namespaces},
		&corev1.Pod{}:                   {Namespaces: namespaces},
		&corev1.PersistentVolumeClaim{}: {Namespaces: namespaces},
		&appsv1.ReplicaSet{}:            {Namespaces: namespaces},
		&appsv1.Deployment{}:            {Namespaces: namespaces},
		&appsv1.StatefulSet{}:           {Namespaces: namespaces},
		&appsv1.DaemonSet{}:             {Namespaces: namespaces},
		&batchv1.Job{}:                  {Namespaces: namespaces},
		&batchv1.CronJob{}:              {Namespaces: namespaces},
	}
	if nodePages != ModeIgnore {
		// A Node is cluster-scoped, so ByObject.Namespaces must stay unset.
		observed[&corev1.Node{}] = cache.ByObject{}
	}
	return observed
}

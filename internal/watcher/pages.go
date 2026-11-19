package watcher

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/cooldown"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/verification"
)

// AvailabilityReader reports the replica health of a resolved Subject. It is how
// an Event, which carries no replica counts, still gets the availability
// modifier that decides whether a noisy replica is worth a Page.
type AvailabilityReader interface {
	Availability(ctx context.Context, subject Subject) (Availability, error)
}

// Pipeline turns a normalized Signal into an observation, a join on an Active
// Page, or a new Page. It is the single place that reads the Cooldown and
// writes the Page, so the join rule and the suppression counter cannot diverge.
type Pipeline struct {
	// Client creates Pages and reads them by label.
	Client client.Client
	// Status is the only writer of Page status.
	Status *page.StatusStore
	// Resolver canonicalizes a raw object to its Subject.
	Resolver *Resolver
	// Availability reports a Subject's replica health on the Event path.
	Availability AvailabilityReader
	// Store is the durable Cooldown and observation state.
	Store *cooldown.Store
	// Policy decides watch, warn-only, or ignore.
	Policy Policy
	// Gate decides severity, settling, aggregation, and the burst ceiling.
	Gate Gate
	// Clock stamps every decision. It defaults to the real clock.
	Clock clock.PassiveClock
	// Namespace holds Pages.
	Namespace string
	// ClusterID identifies the Cluster in every Fingerprint.
	ClusterID string
	// MaxConcurrentPages bounds running investigations. Zero leaves it unbounded.
	MaxConcurrentPages int
	// MaxPagesPerHour bounds investigations started per sliding hour. Zero leaves it unbounded.
	MaxPagesPerHour int
	// Observe tunes the status observers' thresholds.
	Observe ObserveConfig
}

// now returns the pipeline clock.
func (p *Pipeline) now() time.Time {
	if p.Clock == nil {
		return time.Now()
	}
	return p.Clock.Now()
}

// gate returns the gate with every default filled.
func (p *Pipeline) gate() Gate { return p.Gate.withDefaults() }

// HandleEvent ingests one Warning Event. It resolves the Event's object to its
// canonical Subject, then applies the same rule the status path applies.
func (p *Pipeline) HandleEvent(ctx context.Context, signal Signal) error {
	if signal.Reference.Kind == "" || signal.Reference.Name == "" {
		return fmt.Errorf("event signal names no object: %w", ErrNoSubject)
	}
	subject, err := p.Resolver.Resolve(ctx, signal.Reference)
	if err != nil {
		return err
	}
	if signal.ResolvedFrom != "" && subject.ResolvedFrom == "" {
		subject.ResolvedFrom = signal.ResolvedFrom
	}
	availability := Availability{}
	if p.Availability != nil {
		if resolved, err := p.Availability.Availability(ctx, subject); err == nil {
			availability = resolved
		}
	}
	return p.handle(ctx, subject, availability, signal, false)
}

// HandleStatus ingests every live signal a status observer derived from one
// Subject in one reconcile. Status is authoritative: a signal that is no longer
// derived stops advancing its observation's last-seen time, which is what clears
// it, and a Page clears from live status rather than from a resolution Event.
func (p *Pipeline) HandleStatus(ctx context.Context, subject Subject, availability Availability,
	signals []Signal,
) error {
	for _, signal := range signals {
		if err := p.handle(ctx, subject, availability, signal, true); err != nil {
			if errors.Is(err, ErrNoSubject) || errors.Is(err, ErrIgnored) {
				continue
			}
			return err
		}
	}
	return nil
}

// handle applies the pipeline to one signal. status reports whether the signal
// came from durable status, which skips aggregation and never re-joins a
// Fingerprint the Page already records.
func (p *Pipeline) handle(ctx context.Context, subject Subject, availability Availability,
	signal Signal, status bool,
) error {
	verdict := p.Policy.Evaluate(subject)
	if verdict.Ignored() {
		return nil
	}
	entry, known := LookupEntry(signal.Class)
	if !known {
		return nil
	}
	now := p.now()
	signal = signal.normalized(now)
	subject.ClusterID = p.ClusterID
	subjectKey := SubjectKey(subject)
	fingerprint := FailureFingerprint(subject, signal.Class, signal.Scope)

	previous, found, err := p.Store.Observation(ctx, fingerprint)
	if err != nil {
		return err
	}
	record, recordFound, err := p.Store.Get(ctx, fingerprint)
	if err != nil {
		return err
	}
	suppressed := int64(0)
	if recordFound {
		suppressed = record.SuppressedCount
	}
	occurrences, firstSeen, lastCount := p.window(previous, found, signal, now)
	severity := p.gate().Severity(entry, availability, occurrences, suppressed)
	active, err := p.activePage(ctx, subjectKey, subject.UID)
	if err != nil {
		return err
	}
	observe := func(state v1alpha1.FingerprintState, lastTrigger time.Time) error {
		return p.putObservation(ctx, subject, fingerprint, previous, found, severity, occurrences,
			lastCount, firstSeen, state, lastTrigger, signal.Note, now)
	}
	paged := v1alpha1.FingerprintObserving
	if active != nil {
		paged = v1alpha1.FingerprintPaged
	}

	// A repeat inside the aggregation window increments occurrences without
	// emitting a second Trigger: a four-hundred-restart crash loop stays one Page.
	if !status && found && p.gate().Aggregated(previous.LastTriggerAt, now) {
		return observe(paged, time.Time{})
	}
	if !p.gate().Settled(entry, firstSeen, now, signal.Immediate) {
		return observe(v1alpha1.FingerprintObserving, time.Time{})
	}
	if !p.gate().WithinBurst(occurrences) {
		return observe(paged, time.Time{})
	}
	if !p.gate().Pageable(severity) {
		// An info observation is retained as evidence; it never opens a Page by itself.
		if active != nil {
			return p.join(ctx, active, subject, fingerprint, entry, signal, severity, occurrences,
				v1alpha1.FingerprintPaged, false)
		}
		return observe(v1alpha1.FingerprintObserving, time.Time{})
	}

	if active != nil {
		if status && recordsFingerprint(active, fingerprint) {
			// Status re-derivation must not inflate the Trigger count.
			return observe(v1alpha1.FingerprintPaged, time.Time{})
		}
		inCooldown := recordFound && now.Before(record.CooldownUntil)
		return p.join(ctx, active, subject, fingerprint, entry, signal, severity, occurrences,
			v1alpha1.FingerprintPaged, inCooldown)
	}

	opened, err := p.Store.BeginPage(ctx, cooldown.Record{
		Fingerprint: fingerprint,
		ClusterID:   p.ClusterID,
		SubjectKey:  subjectKey,
		APIVersion:  subject.APIVersion,
		Kind:        subject.Kind,
		Namespace:   subject.Namespace,
		Name:        subject.Name,
		UID:         subject.UID,
		ReasonClass: string(signal.Class),
		Scope:       signal.Scope,
		FirstSeenAt: firstSeen,
	}, now)
	if errors.Is(err, cooldown.ErrNotOpen) {
		return observe(v1alpha1.FingerprintObserving, time.Time{})
	}
	if err != nil {
		return err
	}
	if err := p.createPage(ctx, subject, subjectKey, fingerprint, entry, signal, severity, occurrences,
		opened, verdict, now); err != nil {
		return err
	}
	return observe(v1alpha1.FingerprintPaged, now)
}

// window computes the occurrence count, the first-seen time, and the last Event
// count of an observation. Occurrences restart when the aggregation window
// closes, so escalation measures one window rather than a Page's lifetime.
func (p *Pipeline) window(previous cooldown.Observation, found bool, signal Signal,
	now time.Time,
) (occurrences int64, firstSeen time.Time, lastCount int64) {
	firstSeen, lastCount = signal.FirstSeen, signal.Count
	if !found {
		return occurrenceDelta(0, signal.Count), firstSeen, lastCount
	}
	firstSeen = previous.FirstSeenAt
	if previous.State == cooldown.StateResolved ||
		now.Sub(previous.LastTriggerAt) >= p.gate().AggregationWindow {
		firstSeen = signal.FirstSeen
		occurrences = 0
	} else {
		occurrences = previous.Occurrences
	}
	if signal.Count > 0 {
		lastCount = signal.Count
		return occurrences + occurrenceDelta(previous.LastCount, signal.Count), firstSeen, lastCount
	}
	return occurrences + 1, firstSeen, previous.LastCount
}

// putObservation persists one observation, coalescing heartbeats.
func (p *Pipeline) putObservation(ctx context.Context, subject Subject, fingerprint string,
	previous cooldown.Observation, found bool, severity v1alpha1.Severity,
	occurrences, lastCount int64, firstSeen time.Time, state v1alpha1.FingerprintState,
	lastTrigger time.Time, note string, now time.Time,
) error {
	consecutive := int64(1)
	if found && previous.State != cooldown.StateResolved {
		consecutive = previous.Consecutive + 1
	}
	if lastTrigger.IsZero() && found && previous.State == cooldown.StatePaged {
		lastTrigger = previous.LastTriggerAt
	}
	_, err := p.Store.PutObservation(ctx, cooldown.Observation{
		Fingerprint:   fingerprint,
		SubjectKey:    SubjectKey(subject),
		State:         string(state),
		Severity:      string(severity),
		FirstSeenAt:   firstSeen,
		LastSeenAt:    now,
		Consecutive:   consecutive,
		Occurrences:   occurrences,
		LastCount:     lastCount,
		LastNote:      note,
		LastTriggerAt: lastTrigger,
	}, now)
	return err
}

// join adds a Trigger to an existing Active Page. The Trigger count, the last
// Trigger time, the severity, and the distinct Fingerprint move; no new Page is
// created even when the reason differs. A Trigger inside its Cooldown is still
// recorded, and its suppression is counted on the SQLite row and on the Page.
func (p *Pipeline) join(ctx context.Context, active *v1alpha1.Page, subject Subject, fingerprint string,
	entry Entry, signal Signal, severity v1alpha1.Severity, occurrences int64,
	state v1alpha1.FingerprintState, inCooldown bool,
) error {
	now := p.now()
	suppressed := int64(0)
	if inCooldown {
		count, err := p.Store.Suppress(ctx, fingerprint, now)
		if err != nil {
			return err
		}
		suppressed = count
	}
	_, err := p.Status.Update(ctx, client.ObjectKeyFromObject(active), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase.IsTerminal() {
			return nil
		}
		latest.Status.TriggerCount++
		stamp := metav1.NewTime(now)
		latest.Status.LastTriggerAt = &stamp
		latest.Status.Severity = maximum(latest.Status.Severity, severity)
		if latest.Status.Subject == nil {
			latest.Status.Subject = subject.Status()
		}
		latest.Status.Fingerprints = mergeFingerprint(latest.Status.Fingerprints, fingerprint, entry,
			signal.Scope, occurrences, state)
		if inCooldown {
			latest.Status.SuppressedCount = int32(suppressed)
		}
		return nil
	})
	return err
}

// mergeFingerprint records a Fingerprint on a Page, capped so a crash storm
// cannot grow the Page without bound.
func mergeFingerprint(entries []v1alpha1.FingerprintEntry, fingerprint string, entry Entry, scope string,
	occurrences int64, state v1alpha1.FingerprintState,
) []v1alpha1.FingerprintEntry {
	const maxFingerprints = 20
	for i := range entries {
		if entries[i].Fingerprint != fingerprint {
			continue
		}
		entries[i].State = state
		if occurrences > int64(entries[i].Occurrences) {
			entries[i].Occurrences = int32(occurrences)
		}
		return entries
	}
	if len(entries) >= maxFingerprints {
		return entries
	}
	return append(entries, v1alpha1.FingerprintEntry{
		Fingerprint: fingerprint,
		ReasonClass: string(entry.Class),
		Scope:       scope,
		State:       state,
		Occurrences: int32(occurrences),
	})
}

// recordsFingerprint reports whether a Page already records a Fingerprint.
func recordsFingerprint(p *v1alpha1.Page, fingerprint string) bool {
	for _, entry := range p.Status.Fingerprints {
		if entry.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

// createPage creates the Page and mirrors the Cooldown and the guardrail
// decision onto its status. The Cooldown row was already written, so a crash
// between the two suppresses the next Trigger instead of duplicating the Page.
func (p *Pipeline) createPage(ctx context.Context, subject Subject, subjectKey, fingerprint string,
	entry Entry, signal Signal, severity v1alpha1.Severity, occurrences int64, opened cooldown.Window,
	verdict Verdict, now time.Time,
) error {
	previous, excluded, err := p.previousPage(ctx, subjectKey)
	if err != nil {
		return err
	}
	investigation, err := p.investigationFor(ctx, verdict, now)
	if err != nil {
		return err
	}
	created := &v1alpha1.Page{
		GenerateName: pageNamePrefix(subject),
		Namespace:    p.Namespace,
		Labels:       map[string]string{v1alpha1.LabelSubjectKey: subjectKey},
		Spec: v1alpha1.PageSpec{
			Target:                 subject.Target(),
			Fingerprint:            fingerprint,
			Trigger:                signal.Trigger(),
			PreviousPage:           previous,
			ExcludedProposalHashes: excluded,
		},
	}
	if err := p.Client.Create(ctx, created); err != nil {
		return fmt.Errorf("create a Page for %s: %w", subject.Name, err)
	}
	lastPaged := metav1.NewTime(opened.LastPagedAt)
	nextEligible := metav1.NewTime(opened.CooldownUntil)
	lastTrigger := metav1.NewTime(now)
	_, err = p.Status.Update(ctx, client.ObjectKeyFromObject(created), func(latest *v1alpha1.Page) error {
		if latest.Status.Phase != "" {
			return nil
		}
		latest.Status.Phase = v1alpha1.PhasePending
		latest.Status.Severity = severity
		latest.Status.Subject = subject.Status()
		latest.Status.PrimaryFingerprint = fingerprint
		latest.Status.Fingerprints = mergeFingerprint(nil, fingerprint, entry, signal.Scope, occurrences,
			v1alpha1.FingerprintPaged)
		latest.Status.TriggerCount = 1
		latest.Status.SuppressedCount = int32(opened.SuppressedCount)
		latest.Status.LastTriggerAt = &lastTrigger
		latest.Status.Cooldown = &v1alpha1.CooldownStatus{
			LastPagedAt:     &lastPaged,
			NextEligibleAt:  &nextEligible,
			RecurrenceCount: opened.RecurrenceCount,
			BackoffLevel:    int32(opened.Level),
		}
		latest.Status.Investigation = investigation
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// pageNamePrefix returns a DNS-safe GenerateName prefix for a Subject's Page.
func pageNamePrefix(subject Subject) string {
	name := strings.ToLower(subject.Name)
	var safe strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-':
			safe.WriteRune(char)
		default:
			safe.WriteByte('-')
		}
		if safe.Len() >= 40 {
			break
		}
	}
	prefix := strings.Trim(safe.String(), "-")
	if prefix == "" {
		prefix = "page"
	}
	return prefix + "-"
}

// investigationFor returns the guardrail decision for a new Page, or nil when an
// investigation may start.
func (p *Pipeline) investigationFor(ctx context.Context, verdict Verdict, now time.Time) (
	*v1alpha1.InvestigationStatus, error,
) {
	if verdict.Mode == ModeWarnOnly {
		return &v1alpha1.InvestigationStatus{
			State:  v1alpha1.InvestigationSuppressed,
			Reason: v1alpha1.ReasonWarnOnly,
		}, nil
	}
	if p.MaxPagesPerHour > 0 {
		paged, err := p.Store.PagedSince(ctx, now.Add(-time.Hour))
		if err != nil {
			return nil, err
		}
		if paged > int64(p.MaxPagesPerHour) {
			return &v1alpha1.InvestigationStatus{
				State:  v1alpha1.InvestigationSuppressed,
				Reason: v1alpha1.ReasonHourlyLimit,
			}, nil
		}
	}
	if p.MaxConcurrentPages > 0 {
		running, err := p.runningInvestigations(ctx)
		if err != nil {
			return nil, err
		}
		if running >= p.MaxConcurrentPages {
			return &v1alpha1.InvestigationStatus{
				State:  v1alpha1.InvestigationQueued,
				Reason: v1alpha1.ReasonConcurrencyLimit,
			}, nil
		}
	}
	return nil, nil
}

// runningInvestigations counts the Pages whose agent Job is running.
func (p *Pipeline) runningInvestigations(ctx context.Context) (int, error) {
	var pages v1alpha1.PageList
	if err := p.Client.List(ctx, &pages, client.InNamespace(p.Namespace)); err != nil {
		return 0, fmt.Errorf("list Pages: %w", err)
	}
	running := 0
	for _, candidate := range pages.Items {
		if candidate.Status.Phase == v1alpha1.PhaseInvestigating {
			running++
		}
	}
	return running, nil
}

// pagesForSubject lists every Page that carries the Subject's label.
func (p *Pipeline) pagesForSubject(ctx context.Context, subjectKey string) ([]v1alpha1.Page, error) {
	var pages v1alpha1.PageList
	if err := p.Client.List(ctx, &pages, client.InNamespace(p.Namespace),
		client.MatchingLabels{v1alpha1.LabelSubjectKey: subjectKey}); err != nil {
		return nil, fmt.Errorf("list Pages for a Subject: %w", err)
	}
	return pages.Items, nil
}

// activePage returns the canonical Active Page for a Subject, verified by UID so
// a recreated Target never inherits an old Page. One labelled list is the whole
// lookup; no scan is needed.
func (p *Pipeline) activePage(ctx context.Context, subjectKey, uid string) (*v1alpha1.Page, error) {
	pages, err := p.pagesForSubject(ctx, subjectKey)
	if err != nil {
		return nil, err
	}
	var active []v1alpha1.Page
	for _, candidate := range pages {
		if !candidate.Status.Phase.IsActive() || !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		if candidate.Status.Subject != nil && candidate.Status.Subject.UID != "" &&
			candidate.Status.Subject.UID != uid {
			continue
		}
		active = append(active, candidate)
	}
	if len(active) == 0 {
		return nil, nil
	}
	slices.SortFunc(active, func(a, b v1alpha1.Page) int {
		if order := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); order != 0 {
			return order
		}
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}
		return strings.Compare(string(a.UID), string(b.UID))
	})
	return &active[0], nil
}

// previousPage returns the most recent Page for a Subject, so a Recurrence links
// to the Page that recorded the previous outcome and does not propose the change
// that failed there again.
func (p *Pipeline) previousPage(ctx context.Context, subjectKey string) (string, []string, error) {
	pages, err := p.pagesForSubject(ctx, subjectKey)
	if err != nil || len(pages) == 0 {
		return "", nil, err
	}
	slices.SortFunc(pages, func(a, b v1alpha1.Page) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	previous := pages[len(pages)-1]
	name, excluded := verification.RecurrenceFor(&previous)
	return name, excluded, nil
}

// MarkFingerprintState mirrors an observation state onto every Active Page of a
// Subject that records the Fingerprint, so a Page stops claiming a failure is
// paged once live status stopped deriving it. It is the write that keeps the
// `fingerprints` list a mirror of `failure_observations` rather than a record of
// the first sighting only.
func (p *Pipeline) MarkFingerprintState(ctx context.Context, subjectKey, fingerprint string,
	state v1alpha1.FingerprintState,
) error {
	pages, err := p.pagesForSubject(ctx, subjectKey)
	if err != nil {
		return err
	}
	for i := range pages {
		candidate := &pages[i]
		if !candidate.Status.Phase.IsActive() || !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		if !recordsFingerprint(candidate, fingerprint) {
			continue
		}
		if _, err := p.Status.Update(ctx, client.ObjectKeyFromObject(candidate), func(latest *v1alpha1.Page) error {
			if !latest.Status.Phase.IsActive() {
				return nil
			}
			for index := range latest.Status.Fingerprints {
				if latest.Status.Fingerprints[index].Fingerprint == fingerprint {
					latest.Status.Fingerprints[index].State = state
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// CloseClearedPages closes the Active Page of a Subject whose observations have
// all cleared, so a page does not stay open forever and a later Recurrence opens
// a new Page with the next backoff level. Only a Page that has not reached a
// human decision is closed; an approval or a pull request is never abandoned by
// the watcher.
func (p *Pipeline) CloseClearedPages(ctx context.Context, subjectKey string, now time.Time) error {
	pages, err := p.pagesForSubject(ctx, subjectKey)
	if err != nil {
		return err
	}
	for i := range pages {
		candidate := &pages[i]
		if !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		switch candidate.Status.Phase {
		case v1alpha1.PhasePending, v1alpha1.PhaseInvestigating:
		default:
			continue
		}
		stamp := metav1.NewTime(now)
		_, err := p.Status.Update(ctx, client.ObjectKeyFromObject(candidate), func(latest *v1alpha1.Page) error {
			switch latest.Status.Phase {
			case v1alpha1.PhasePending, v1alpha1.PhaseInvestigating:
			default:
				return nil
			}
			latest.Status.Phase = v1alpha1.PhaseRecovered
			latest.Status.TerminalAt = &stamp
			setCondition(&latest.Status, v1alpha1.ConditionVerified, metav1.ConditionTrue,
				v1alpha1.ReasonRecovered, "the failure cleared from live status", now)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// PromoteQueued starts queued investigations while the concurrency ceiling has
// room, oldest first, so the queue is visible rather than silently dropped.
func (p *Pipeline) PromoteQueued(ctx context.Context) (int, error) {
	if p.MaxConcurrentPages <= 0 {
		return 0, nil
	}
	running, err := p.runningInvestigations(ctx)
	if err != nil {
		return 0, err
	}
	var pages v1alpha1.PageList
	if err := p.Client.List(ctx, &pages, client.InNamespace(p.Namespace)); err != nil {
		return 0, fmt.Errorf("list Pages: %w", err)
	}
	var queued []v1alpha1.Page
	for _, candidate := range pages.Items {
		if candidate.Status.Phase != v1alpha1.PhasePending || candidate.Status.Investigation == nil {
			continue
		}
		if candidate.Status.Investigation.State == v1alpha1.InvestigationQueued {
			queued = append(queued, candidate)
		}
	}
	slices.SortFunc(queued, func(a, b v1alpha1.Page) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	promoted := 0
	for i := range queued {
		if running+promoted >= p.MaxConcurrentPages {
			break
		}
		candidate := &queued[i]
		_, err := p.Status.Update(ctx, client.ObjectKeyFromObject(candidate), func(latest *v1alpha1.Page) error {
			if latest.Status.Phase != v1alpha1.PhasePending || latest.Status.Investigation == nil {
				return nil
			}
			if latest.Status.Investigation.State != v1alpha1.InvestigationQueued {
				return nil
			}
			latest.Status.Investigation = nil
			return nil
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return promoted, err
		}
		promoted++
	}
	return promoted, nil
}

// setCondition sets one Page condition, replacing an earlier one of the same type.
func setCondition(status *v1alpha1.PageStatus, conditionType string, value metav1.ConditionStatus,
	reason, message string, now time.Time,
) {
	for i := range status.Conditions {
		if status.Conditions[i].Type != conditionType {
			continue
		}
		status.Conditions[i].Status = value
		status.Conditions[i].Reason = reason
		status.Conditions[i].Message = message
		status.Conditions[i].LastTransitionTime = metav1.NewTime(now)
		return
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             value,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(now),
	})
}

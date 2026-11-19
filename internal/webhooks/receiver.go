package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/page"
	"github.com/a-hagi613/pagerless/internal/subject"
)

// Path is the one route that accepts every alert dialect.
const Path = "/v1/webhooks/alerts"

const (
	// MaxBodyBytes caps a request body at 1 MiB.
	MaxBodyBytes = 1 << 20
	// DefaultTimeout is the handler's wall-clock ceiling.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxAlerts is the number of alerts decoded from one request.
	DefaultMaxAlerts = 50
	// DefaultRequestsPerSecond is the per-source rate.
	DefaultRequestsPerSecond = 10
	// DefaultBurst is the per-source burst allowance.
	DefaultBurst = 20
	// DeliveryRetention is how long a delivery row is kept.
	DeliveryRetention = time.Hour
	// maxJoinAttempts bounds a join that races the end of an Active Page.
	maxJoinAttempts = 3
)

// errNotActive means the Page an alert tried to join is no longer active.
var errNotActive = errors.New("the Page is no longer active")

// Delivery is the receiver's idempotency record of one alert.
type Delivery struct {
	// Key is the delivery key.
	Key string
	// Source is the sender dialect.
	Source string
	// Sender names the configured sender.
	Sender string
	// Fingerprint is the sender's alert fingerprint.
	Fingerprint string
	// Status is firing or resolved.
	Status string
	// Subject is the resolved subject key, or the ignore reason.
	Subject string
	// Page is the Page the alert opened or joined.
	Page string
	// ReceivedAt is when the receiver accepted the alert.
	ReceivedAt time.Time
	// Outcome is applied or ignored.
	Outcome string
	// Reason explains an ignored alert.
	Reason string
}

// DeliveryStore persists delivery keys and prunes expired ones.
type DeliveryStore interface {
	// HasDelivery reports whether the key is already recorded.
	HasDelivery(ctx context.Context, key string) (bool, error)
	// RecordDelivery records the key and reports whether it is new.
	RecordDelivery(ctx context.Context, delivery Delivery) (bool, error)
	// PruneDeliveries deletes rows received before the given time.
	PruneDeliveries(ctx context.Context, before time.Time) (int64, error)
}

// Receiver accepts every alert dialect, normalises each one, resolves the
// subject with the shared resolver, and joins the Active Page of that subject.
type Receiver struct {
	// Client creates Pages.
	Client client.Client
	// Status records the Trigger on the Active Page.
	Status *page.StatusStore
	// Namespace is the namespace that holds Pages.
	Namespace string
	// Namespaces are the namespaces whose alerts may open or join Pages. An
	// empty list accepts none.
	Namespaces []string
	// Resolver resolves the subject from labels and annotations. It is required.
	Resolver *subject.Resolver
	// Checker fills the live UID and detects a missing Target. It is optional.
	Checker *subject.Checker
	// Deliveries is the idempotency store. It is required.
	Deliveries DeliveryStore
	// Clock defaults to the real clock.
	Clock clock.PassiveClock
	// Timeout bounds one request. It defaults to DefaultTimeout.
	Timeout time.Duration
	// MaxAlerts is the number of alerts decoded from one request. It defaults
	// to DefaultMaxAlerts.
	MaxAlerts int
	// RequestsPerSecond is the per-source rate. It defaults to
	// DefaultRequestsPerSecond.
	RequestsPerSecond float64
	// Burst is the per-source burst allowance. It defaults to DefaultBurst.
	Burst int
	// AlertmanagerToken reads the bearer token of the Alertmanager dialect.
	AlertmanagerToken func() (string, error)
	// GrafanaSecret reads the HMAC-SHA256 secret of the Grafana dialect.
	GrafanaSecret func() (string, error)
	// DatadogToken reads the bearer token of the Datadog dialect.
	DatadogToken func() (string, error)
	// GenericToken reads the bearer token of the generic dialect.
	GenericToken func() (string, error)
	// SNSTopics is the topic allowlist. An empty list refuses every SNS topic,
	// because an unauthenticated topic is not an authenticated sender.
	SNSTopics []string
	// VerifySNSSignature verifies the SNS signature before decoding.
	VerifySNSSignature bool

	mutex     sync.Mutex
	limiters  map[Source]*rateLimiter
	lastPrune time.Time
}

// response is the bounded answer to a sender.
type response struct {
	// Applied is the number of alerts that opened or joined a Page.
	Applied int `json:"applied"`
	// Ignored lists the alerts that were deliberately ignored.
	Ignored []ignoredAlert `json:"ignored,omitempty"`
}

// ignoredAlert names one ignored alert.
type ignoredAlert struct {
	// Index is the alert's position in the payload.
	Index int `json:"index"`
	// Reason is a short, stable explanation.
	Reason string `json:"reason"`
}

// ServeHTTP answers one alert delivery.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	contentType := request.Header.Get("Content-Type")
	if mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0]); mediaType != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()

	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, MaxBodyBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			http.Error(w, fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "the body could not be read", http.StatusBadRequest)
		return
	}
	source := detectedSource(request.Header, body)
	if !r.allow(source) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	alerts, err := decode(decodeRequest{
		ctx: ctx, source: source, body: body, header: request.Header, auth: r.authenticator(), now: r.now(),
	})
	if err != nil {
		if ignore, ok := errors.AsType[*Ignore](err); ok {
			writeResponse(w, response{Ignored: []ignoredAlert{{Index: 0, Reason: ignore.Reason}}})
			return
		}
		if clientError, ok := errors.AsType[*ClientError](err); ok {
			http.Error(w, clientError.Message, clientError.Status)
			return
		}
		logf.FromContext(ctx).Error(err, "decode an alert delivery", "source", source)
		http.Error(w, "the payload could not be decoded", http.StatusBadRequest)
		return
	}
	limit := r.MaxAlerts
	if limit <= 0 {
		limit = DefaultMaxAlerts
	}
	answer := response{}
	for index, alert := range alerts {
		if index >= limit {
			answer.Ignored = append(answer.Ignored, ignoredAlert{Index: index, Reason: "per-request-alert-limit"})
			continue
		}
		name, applied, reason, err := r.apply(ctx, alert)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				logf.FromContext(ctx).Error(err, "apply an alert before the handler deadline", "source", source)
				http.Error(w, "the request exceeded its deadline", http.StatusServiceUnavailable)
				return
			}
			logf.FromContext(ctx).Error(err, "apply an alert", "source", source, "alert", alert.Name)
			http.Error(w, "the alert could not be applied", http.StatusInternalServerError)
			return
		}
		if !applied {
			answer.Ignored = append(answer.Ignored, ignoredAlert{Index: index, Reason: reason})
			continue
		}
		answer.Applied++
		logf.FromContext(ctx).Info("accepted an inbound alert", "source", source, "alert", alert.Name, "page", name)
	}
	r.prune(ctx)
	writeResponse(w, answer)
}

// writeResponse writes the bounded answer with the status the ticket requires:
// 200 when every alert was applied, 202 when some were deliberately ignored.
func writeResponse(w http.ResponseWriter, answer response) {
	status := http.StatusOK
	if len(answer.Ignored) > 0 {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer)
}

// authenticator copies the receiver's configured secrets.
func (r *Receiver) authenticator() authenticator {
	return authenticator{
		tokens: map[Source]secret{
			SourceAlertmanager: r.AlertmanagerToken,
			SourceDatadog:      r.DatadogToken,
			SourceGeneric:      r.GenericToken,
		},
		grafanaSecret: r.GrafanaSecret,
		snsTopics:     r.SNSTopics,
		verifySNS:     r.VerifySNSSignature,
	}
}

// apply resolves the subject, applies the alert, and records its delivery key.
func (r *Receiver) apply(ctx context.Context, alert Alert) (string, bool, string, error) {
	if !subject.IsOptedIn(alert.Labels) {
		return "", false, "not-opted-in", nil
	}
	namespace := alert.Labels[subject.LabelNamespace]
	if !slices.Contains(r.Namespaces, namespace) {
		return "", false, "namespace-not-allowed", nil
	}
	resolved, err := r.Resolver.Resolve(ctx, alert.Labels, alert.Annotations)
	if err != nil {
		if reason, ignored := subject.IgnoreReason(err); ignored {
			return "", false, reason, nil
		}
		return "", false, "", fmt.Errorf("resolve the subject of an alert: %w", err)
	}
	missing := false
	if r.Checker != nil {
		live, found, err := r.Checker.Lookup(ctx, resolved.Subject)
		if err != nil {
			return "", false, "", fmt.Errorf("look up the alert's Target: %w", err)
		}
		if !found {
			missing = true
			resolved.Resolution = subject.ResolutionMissing
		} else {
			resolved.Subject = live
		}
	}
	target := resolved.Subject.TargetReference()
	key := deliveryKey(alert, resolved.Subject.Key())

	r.mutex.Lock()
	defer r.mutex.Unlock()
	duplicate, err := r.Deliveries.HasDelivery(ctx, key)
	if err != nil {
		return "", false, "", fmt.Errorf("read a delivery key: %w", err)
	}
	if duplicate {
		return "", true, "", nil
	}
	var name string
	var applied bool
	var reason string
	if alert.Status == StatusResolved {
		name, applied, reason, err = r.resolve(ctx, alert, target, missing)
	} else {
		name, applied, reason, err = r.trigger(ctx, alert, resolved, missing)
	}
	if err != nil || !applied {
		return name, applied, reason, err
	}
	outcome := DeliveryOutcomeApplied
	if _, err := r.Deliveries.RecordDelivery(ctx, Delivery{
		Key: key, Source: string(alert.Source), Sender: alert.Sender, Fingerprint: alert.Fingerprint,
		Status: string(alert.Status), Subject: resolved.Subject.Key(), Page: name,
		ReceivedAt: r.now(), Outcome: outcome,
	}); err != nil {
		return name, false, "", fmt.Errorf("record a delivery key: %w", err)
	}
	return name, true, "", nil
}

// DeliveryOutcomeApplied is the outcome of an accepted alert.
const DeliveryOutcomeApplied = "applied"

// trigger opens a Page for the subject or joins its Active Page.
func (r *Receiver) trigger(
	ctx context.Context, alert Alert, resolved subject.Result, missing bool,
) (string, bool, string, error) {
	target := resolved.Subject.TargetReference()
	fingerprint := v1alpha1.Fingerprint(target, "alert:"+alert.Name)
	for range maxJoinAttempts {
		name, found, err := r.activePage(ctx, target, missing)
		if err != nil {
			return "", false, "", err
		}
		if found {
			err := r.join(ctx, name, alert, resolved, missing)
			if errors.Is(err, errNotActive) || apierrors.IsNotFound(err) {
				continue
			}
			return name, err == nil, "", err
		}
		created := r.newPage(target, fingerprint, alert, missing)
		err = r.Client.Create(ctx, created)
		if err == nil {
			if err := r.recordStatus(ctx, created.Name, alert, resolved, missing); err != nil {
				return created.Name, false, "", err
			}
			return created.Name, true, "", nil
		}
		if apierrors.IsInvalid(err) {
			return "", false, "unrepresentable-subject", nil
		}
		if apierrors.IsAlreadyExists(err) {
			continue
		}
		return "", false, "", fmt.Errorf("create Page: %w", err)
	}
	return "", false, "", fmt.Errorf("the Active Page of %s/%s ended during %d attempts to join it",
		target.Namespace, target.Name, maxJoinAttempts)
}

// resolve closes only the alert Trigger that matches, and leaves the Page open
// while Kubernetes observations remain.
func (r *Receiver) resolve(
	ctx context.Context, alert Alert, target v1alpha1.TargetReference, missing bool,
) (string, bool, string, error) {
	name, found, err := r.activePage(ctx, target, missing)
	if err != nil {
		return "", false, "", err
	}
	if !found {
		return "", false, "no-active-page", nil
	}
	closed := true
	_, err = r.Status.Update(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name},
		func(latest *v1alpha1.Page) error {
			for index := range latest.Status.Alerts {
				if !sameAlert(latest.Status.Alerts[index], alert) {
					continue
				}
				latest.Status.Alerts[index].State = v1alpha1.AlertStateResolved
				end := metav1.NewTime(alertTime(alert.EndsAt, r.now()))
				latest.Status.Alerts[index].EndsAt = &end
				closed = true
				return nil
			}
			closed = false
			return nil
		})
	if err != nil {
		return name, false, "", err
	}
	if !closed {
		return name, false, "trigger-not-found", nil
	}
	return name, true, "", nil
}

// join counts one more Trigger on the named Page and records the alert.
func (r *Receiver) join(
	ctx context.Context, name string, alert Alert, resolved subject.Result, missing bool,
) error {
	_, err := r.Status.Update(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name},
		func(latest *v1alpha1.Page) error {
			if latest.Status.Phase != "" && latest.Status.Phase.IsTerminal() {
				return errNotActive
			}
			latest.Status.TriggerCount++
			now := metav1.NewTime(r.now())
			latest.Status.LastTriggerAt = &now
			mergeAlert(latest, alert)
			latest.Status.Severity = maxSeverity(latest.Status.Severity, alert.Severity)
			if latest.Status.Subject == nil {
				latest.Status.Subject = subjectStatus(resolved, missing)
			}
			return nil
		})
	return err
}

// recordStatus records the resolved subject and the alert on a new Page.
func (r *Receiver) recordStatus(
	ctx context.Context, name string, alert Alert, resolved subject.Result, missing bool,
) error {
	_, err := r.Status.Update(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name},
		func(latest *v1alpha1.Page) error {
			latest.Status.TriggerCount++
			latest.Status.Severity = maxSeverity(latest.Status.Severity, alert.Severity)
			latest.Status.Subject = subjectStatus(resolved, missing)
			now := metav1.NewTime(r.now())
			latest.Status.LastTriggerAt = &now
			mergeAlert(latest, alert)
			return nil
		})
	return err
}

// activePage returns the canonical Active Page of the subject. A missing
// Target only matches a Page that already records the Target as missing, so a
// same-named object elsewhere is never attached.
func (r *Receiver) activePage(
	ctx context.Context, target v1alpha1.TargetReference, missing bool,
) (string, bool, error) {
	var pages v1alpha1.PageList
	if err := r.Client.List(ctx, &pages, client.InNamespace(r.Namespace)); err != nil {
		return "", false, fmt.Errorf("list Pages: %w", err)
	}
	for index := range pages.Items {
		p := &pages.Items[index]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		if p.Status.Phase != "" && !p.Status.Phase.IsActive() {
			continue
		}
		if !sameTarget(p.Spec.Target, target) {
			continue
		}
		if missing && (p.Status.Subject == nil ||
			p.Status.Subject.Resolution != v1alpha1.ResolutionMissing) {
			continue
		}
		return p.Name, true, nil
	}
	return "", false, nil
}

// sameTarget reports whether two references name the same object. A UID on
// both sides must match; a name match alone never merges two objects.
func sameTarget(a, b v1alpha1.TargetReference) bool {
	if a.Kind != b.Kind || a.Namespace != b.Namespace || a.Name != b.Name {
		return false
	}
	if a.UID != "" && b.UID != "" && a.UID != b.UID {
		return false
	}
	return true
}

// newPage returns the Page that an alert opens.
func (r *Receiver) newPage(
	target v1alpha1.TargetReference, fingerprint string, alert Alert, missing bool,
) *v1alpha1.Page {
	trigger := v1alpha1.Trigger{
		Source: triggerSource(alert.Source), Reason: alert.Name, Message: alert.Message,
		Pod: alert.Labels[subject.LabelPod], AlertFingerprint: alert.Fingerprint,
	}
	if missing {
		trigger.Message = firstNonEmpty(trigger.Message, "the alert names a Target that no longer exists")
	}
	return &v1alpha1.Page{
		Name: v1alpha1.PageName(target, fingerprint), Namespace: r.Namespace,
		Spec: v1alpha1.PageSpec{Target: target, Fingerprint: fingerprint, Trigger: trigger},
	}
}

// subjectStatus records how the subject was resolved.
func subjectStatus(resolved subject.Result, missing bool) *v1alpha1.SubjectStatus {
	resolution := resolved.Resolution
	if missing {
		resolution = subject.ResolutionMissing
	}
	return &v1alpha1.SubjectStatus{
		APIVersion: resolved.Subject.APIVersion, Kind: resolved.Subject.Kind,
		Namespace: resolved.Subject.Namespace, Name: resolved.Subject.Name,
		UID: resolved.Subject.UID, Resolution: v1alpha1.SubjectResolution(resolution),
		ResolvedFromKeys: resolved.ResolvedFrom,
	}
}

// mergeAlert records or refreshes the alert in the bounded status list.
func mergeAlert(latest *v1alpha1.Page, alert Alert) {
	entry := alertTrigger(alert)
	for index := range latest.Status.Alerts {
		if !sameAlert(latest.Status.Alerts[index], alert) {
			continue
		}
		entry.StartsAt = firstTime(latest.Status.Alerts[index].StartsAt, entry.StartsAt)
		latest.Status.Alerts[index] = entry
		return
	}
	if len(latest.Status.Alerts) >= v1alpha1.MaxAlerts {
		latest.Status.Alerts = latest.Status.Alerts[len(latest.Status.Alerts)-v1alpha1.MaxAlerts+1:]
	}
	latest.Status.Alerts = append(latest.Status.Alerts, entry)
}

// alertTrigger converts a normalized alert to its status entry.
func alertTrigger(alert Alert) v1alpha1.AlertTrigger {
	entry := v1alpha1.AlertTrigger{
		Source: triggerSource(alert.Source), Sender: alert.Sender, Fingerprint: alert.Fingerprint,
		Severity: alert.Severity, State: v1alpha1.AlertStateFiring,
	}
	if alert.Status == StatusResolved {
		entry.State = v1alpha1.AlertStateResolved
	}
	if !alert.StartsAt.IsZero() {
		start := metav1.NewTime(alert.StartsAt)
		entry.StartsAt = &start
	}
	if !alert.EndsAt.IsZero() {
		end := metav1.NewTime(alert.EndsAt)
		entry.EndsAt = &end
	}
	return entry
}

// sameAlert reports whether a status entry belongs to an alert.
func sameAlert(entry v1alpha1.AlertTrigger, alert Alert) bool {
	return entry.Source == triggerSource(alert.Source) && entry.Sender == alert.Sender &&
		entry.Fingerprint == alert.Fingerprint
}

// firstTime returns the first non-nil time.
func firstTime(first, second *metav1.Time) *metav1.Time {
	if first != nil {
		return first
	}
	return second
}

// alertTime returns at, or the fallback when at is zero.
func alertTime(at time.Time, fallback time.Time) time.Time {
	if at.IsZero() {
		return fallback
	}
	return at
}

// maxSeverity returns the more urgent severity.
func maxSeverity(a, b v1alpha1.Severity) v1alpha1.Severity {
	rank := map[v1alpha1.Severity]int{
		v1alpha1.SeverityInfo: 1, v1alpha1.SeverityWarning: 2, v1alpha1.SeverityCritical: 3,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// triggerSource maps a dialect to the Page's Trigger vocabulary.
func triggerSource(source Source) v1alpha1.TriggerSource {
	switch source {
	case SourceAlertmanager:
		return v1alpha1.TriggerSourceAlertmanager
	case SourceGrafana:
		return v1alpha1.TriggerSourceGrafana
	case SourceDatadog:
		return v1alpha1.TriggerSourceDatadog
	case SourceSNS:
		return v1alpha1.TriggerSourceSNS
	default:
		return v1alpha1.TriggerSourceAPI
	}
}

// deliveryKey builds the idempotency key over source, sender fingerprint,
// status, start time, and subject.
func deliveryKey(alert Alert, subjectKey string) string {
	digest := sha256.New()
	for _, field := range []string{
		"pagerless.dev/delivery/v1", string(alert.Source), alert.Sender, alert.Fingerprint,
		string(alert.Status), alertTime(alert.StartsAt, time.Time{}).UTC().Format(time.RFC3339Nano), subjectKey,
	} {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

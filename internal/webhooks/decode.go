// Package webhooks is the inbound alert receiver of pagerless-server. It
// accepts the five dialects an Operator configures, normalises them to one
// Trigger vocabulary, resolves the subject through the shared resolver, and
// joins the Active Page of that subject rather than opening a duplicate
// investigation (ADR-0040).
package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/subject"
)

// Source identifies one inbound webhook dialect.
type Source string

const (
	// SourceAlertmanager is the Alertmanager webhook.
	SourceAlertmanager Source = "alertmanager"
	// SourceGrafana is the Grafana unified-alerting webhook.
	SourceGrafana Source = "grafana"
	// SourceDatadog is the Datadog webhook.
	SourceDatadog Source = "datadog"
	// SourceSNS is the AWS SNS and CloudWatch webhook.
	SourceSNS Source = "sns"
	// SourceGeneric is the generic JSON webhook.
	SourceGeneric Source = "generic"
)

// SourceHeader selects a dialect explicitly. Without it the receiver detects
// the dialect from the sender's headers and payload shape.
const SourceHeader = "X-Pagerless-Source"

// Status is the state an alert reports.
type Status string

const (
	// StatusFiring means the alert is firing.
	StatusFiring Status = "firing"
	// StatusResolved means the alert resolved.
	StatusResolved Status = "resolved"
)

// Alert is the normalized form of every dialect.
type Alert struct {
	// Source is the dialect that delivered the alert.
	Source Source
	// Sender is the configured sender name, such as alertmanager.
	Sender string
	// Status is firing or resolved.
	Status Status
	// Name is the alert's name or rule title.
	Name string
	// Fingerprint is the sender's stable identity, computed when absent.
	Fingerprint string
	// Severity is the mapped Page severity.
	Severity v1alpha1.Severity
	// StartsAt is when the alert started firing.
	StartsAt time.Time
	// EndsAt is when the alert resolved.
	EndsAt time.Time
	// Labels carry the sender's labels, including any subject labels.
	Labels map[string]string
	// Annotations carry the sender's annotations, including a subject annotation.
	Annotations map[string]string
	// Message is the human-readable summary.
	Message string
}

// ClientError is a request error that maps to an HTTP status.
type ClientError struct {
	// Status is the HTTP status to return.
	Status int
	// Message is safe to return to the sender: it names no configuration and
	// no credential.
	Message string
}

// Error implements error.
func (e *ClientError) Error() string { return e.Message }

// BadRequest returns a 400 error.
func BadRequest(format string, args ...any) error {
	return &ClientError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

// Unauthorized returns a 401 error.
func Unauthorized(message string) error {
	return &ClientError{Status: http.StatusUnauthorized, Message: message}
}

// Forbidden returns a 403 error.
func Forbidden(message string) error {
	return &ClientError{Status: http.StatusForbidden, Message: message}
}

// ClientStatus returns the HTTP status a decoder error maps to.
func ClientStatus(err error) int {
	if clientError, ok := errors.AsType[*ClientError](err); ok {
		return clientError.Status
	}
	return http.StatusBadRequest
}

// secret is the authentication material of one dialect. A nil function means
// the dialect is not configured, and every delivery of it is refused.
type secret func() (string, error)

// authenticator answers the authentication scheme of each dialect.
type authenticator struct {
	// tokens holds the bearer token sources of the token dialects.
	tokens map[Source]secret
	// grafanaSecret is the HMAC-SHA256 secret of the Grafana webhook.
	grafanaSecret secret
	// snsTopics is the topic allowlist of the SNS webhook.
	snsTopics []string
	// verifySNS reports whether an SNS signature must be verified.
	verifySNS bool
}

// decodeRequest is one authenticated webhook body.
type decodeRequest struct {
	ctx    context.Context
	source Source
	body   []byte
	header http.Header
	auth   authenticator
	now    time.Time
}

// decode dispatches to one dialect decoder.
func decode(request decodeRequest) ([]Alert, error) {
	switch request.source {
	case SourceAlertmanager:
		if err := request.bearer(SourceAlertmanager); err != nil {
			return nil, err
		}
		return decodeAlertmanager(request)
	case SourceGrafana:
		if err := request.verifyGrafana(); err != nil {
			return nil, err
		}
		return decodeGrafana(request)
	case SourceDatadog:
		if err := request.bearer(SourceDatadog); err != nil {
			return nil, err
		}
		return decodeDatadog(request)
	case SourceSNS:
		return decodeSNS(request)
	case SourceGeneric:
		if err := request.bearer(SourceGeneric); err != nil {
			return nil, err
		}
		return decodeGeneric(request)
	default:
		return nil, BadRequest("unknown alert source")
	}
}

// detectedSource returns the dialect the request names or that its headers and
// payload shape identify.
func detectedSource(header http.Header, body []byte) Source {
	if named := Source(header.Get(SourceHeader)); named != "" {
		return named
	}
	if header.Get("X-Amz-Sns-Message-Type") != "" {
		return SourceSNS
	}
	if signature := header.Get("X-Pagerless-Signature"); signature != "" {
		return SourceGrafana
	}
	var probe struct {
		Alerts []map[string]any  `json:"alerts"`
		Status string            `json:"status"`
		Type   string            `json:"Type"`
		ID     string            `json:"id"`
		Title  string            `json:"title"`
		Labels map[string]string `json:"labels"`
		Name   string            `json:"name"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return SourceGeneric
	}
	switch {
	case probe.Type == snsNotificationType || strings.Contains(string(body), `"Type":"`+snsNotificationType+`"`):
		return SourceSNS
	case probe.ID != "" && (probe.Title != "" || probe.Status != ""):
		return SourceDatadog
	case probe.Name != "" && probe.Labels != nil && probe.Status != "":
		return SourceGeneric
	case len(probe.Alerts) > 0:
		return SourceAlertmanager
	default:
		return SourceGeneric
	}
}

// bearer verifies the Authorization header of a token dialect.
func (request decodeRequest) bearer(source Source) error {
	token, ok := request.auth.tokens[source]
	if !ok || token == nil {
		return Forbidden("this alert source is not configured")
	}
	expected, err := token()
	if err != nil || expected == "" {
		return Forbidden("this alert source is not configured")
	}
	provided, bearer := strings.CutPrefix(request.header.Get("Authorization"), "Bearer ")
	want, got := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(provided))
	if !bearer || subtleCompare(want[:], got[:]) != 1 {
		return Unauthorized("unauthorized")
	}
	return nil
}

// verifyGrafana checks the HMAC-SHA256 signature of a Grafana delivery.
func (request decodeRequest) verifyGrafana() error {
	if request.auth.grafanaSecret == nil {
		return Forbidden("this alert source is not configured")
	}
	secret, err := request.auth.grafanaSecret()
	if err != nil || secret == "" {
		return Forbidden("this alert source is not configured")
	}
	signature := request.header.Get("X-Pagerless-Signature")
	if signature == "" {
		signature = request.header.Get("X-Grafana-Signature")
	}
	signature = strings.TrimPrefix(signature, "sha256=")
	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) == 0 {
		return Unauthorized("unauthorized")
	}
	mac := hmacSHA256([]byte(secret), request.body)
	if subtleCompare(mac, provided) != 1 {
		return Unauthorized("unauthorized")
	}
	return nil
}

// alertmanagerPayload is the Alertmanager webhook, version 4.
type alertmanagerPayload struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
		EndsAt      string            `json:"endsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// decodeAlertmanager decodes an Alertmanager delivery.
func decodeAlertmanager(request decodeRequest) ([]Alert, error) {
	var payload alertmanagerPayload
	if err := json.Unmarshal(request.body, &payload); err != nil {
		return nil, BadRequest("malformed Alertmanager payload: %v", err)
	}
	if len(payload.Alerts) == 0 {
		return nil, BadRequest("the Alertmanager payload carries no alerts")
	}
	alerts := make([]Alert, 0, len(payload.Alerts))
	for _, entry := range payload.Alerts {
		alert := Alert{
			Source: SourceAlertmanager, Sender: string(SourceAlertmanager),
			Status: statusOf(entry.Status, payload.Status), Name: entry.Labels["alertname"],
			Fingerprint: entry.Fingerprint, Severity: severityOf(entry.Labels["severity"]),
			Labels: copyMap(entry.Labels), Annotations: copyMap(entry.Annotations),
			Message: firstNonEmpty(entry.Annotations["summary"], entry.Annotations["description"]),
		}
		alert.StartsAt = parseTime(entry.StartsAt)
		alert.EndsAt = parseTime(entry.EndsAt)
		if alert.Fingerprint == "" {
			alert.Fingerprint = stableFingerprint(alert)
		}
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

// grafanaPayload is the Grafana unified-alerting webhook.
type grafanaPayload struct {
	Status string `json:"status"`
	Title  string `json:"title"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    string            `json:"startsAt"`
		EndsAt      string            `json:"endsAt"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

// decodeGrafana decodes a Grafana delivery.
func decodeGrafana(request decodeRequest) ([]Alert, error) {
	var payload grafanaPayload
	if err := json.Unmarshal(request.body, &payload); err != nil {
		return nil, BadRequest("malformed Grafana payload: %v", err)
	}
	if len(payload.Alerts) == 0 {
		return nil, BadRequest("the Grafana payload carries no alerts")
	}
	alerts := make([]Alert, 0, len(payload.Alerts))
	for _, entry := range payload.Alerts {
		alert := Alert{
			Source: SourceGrafana, Sender: string(SourceGrafana),
			Status: statusOf(entry.Status, payload.Status), Name: entry.Labels["alertname"],
			Fingerprint: entry.Fingerprint, Severity: severityOf(entry.Labels["severity"]),
			Labels: copyMap(entry.Labels), Annotations: copyMap(entry.Annotations),
			Message: firstNonEmpty(entry.Annotations["summary"], entry.Annotations["description"], payload.Title),
		}
		alert.StartsAt = parseTime(entry.StartsAt)
		alert.EndsAt = parseTime(entry.EndsAt)
		if alert.Fingerprint == "" {
			alert.Fingerprint = stableFingerprint(alert)
		}
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

// datadogPayload is the payload the shipped Datadog template posts.
type datadogPayload struct {
	ID          string `json:"id"`
	AlertID     string `json:"alert_id"`
	Title       string `json:"title"`
	AlertType   string `json:"alert_type"`
	AlertStatus string `json:"alert_status"`
	Priority    string `json:"priority"`
	Date        int64  `json:"date"`
	Hostname    string `json:"hostname"`
	Tags        string `json:"tags"`
	Body        string `json:"body"`
	URL         string `json:"url"`
}

// DatadogPayloadTemplate is the Datadog webhook payload template to install.
// It carries the alert identity, status, severity, timing, and tags that the
// receiver needs to normalise the alert.
const DatadogPayloadTemplate = `{
  "id": "$ID",
  "alert_id": "$ALERT_ID",
  "title": "$EVENT_TITLE",
  "alert_type": "$ALERT_TYPE",
  "alert_status": "$ALERT_STATUS",
  "priority": "$PRIORITY",
  "date": $DATE,
  "hostname": "$HOSTNAME",
  "tags": "$TAGS",
  "body": "$EVENT_MSG",
  "url": "$LINK"
}`

// decodeDatadog decodes a Datadog delivery.
func decodeDatadog(request decodeRequest) ([]Alert, error) {
	var payload datadogPayload
	if err := json.Unmarshal(request.body, &payload); err != nil {
		return nil, BadRequest("malformed Datadog payload: %v", err)
	}
	if payload.Title == "" && payload.ID == "" {
		return nil, BadRequest("the Datadog payload carries no alert")
	}
	labels := parseDatadogTags(payload.Tags)
	if payload.Hostname != "" {
		labels["host"] = payload.Hostname
	}
	alert := Alert{
		Source: SourceDatadog, Sender: string(SourceDatadog),
		Status:      datadogStatus(payload.AlertStatus, payload.AlertType),
		Name:        firstNonEmpty(labels["alertname"], payload.Title),
		Fingerprint: firstNonEmpty(payload.ID, payload.AlertID),
		Severity:    datadogSeverity(payload.Priority, payload.AlertType),
		Labels:      labels, Annotations: map[string]string{"summary": payload.Body},
		Message: payload.Body,
	}
	if payload.Date > 0 {
		alert.StartsAt = time.Unix(payload.Date, 0).UTC()
	}
	if alert.Fingerprint == "" {
		alert.Fingerprint = stableFingerprint(alert)
	}
	return []Alert{alert}, nil
}

// genericPayload is the generic webhook.
type genericPayload struct {
	Status      string            `json:"status"`
	Name        string            `json:"name"`
	Fingerprint string            `json:"fingerprint"`
	Severity    string            `json:"severity"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Message     string            `json:"message"`
	Alerts      []struct {
		Status      string            `json:"status"`
		Name        string            `json:"name"`
		Fingerprint string            `json:"fingerprint"`
		Severity    string            `json:"severity"`
		StartsAt    string            `json:"startsAt"`
		EndsAt      string            `json:"endsAt"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Message     string            `json:"message"`
	} `json:"alerts"`
}

// decodeGeneric decodes a generic delivery: one alert, or an array of them.
func decodeGeneric(request decodeRequest) ([]Alert, error) {
	var payload genericPayload
	if err := json.Unmarshal(request.body, &payload); err != nil {
		return nil, BadRequest("malformed generic payload: %v", err)
	}
	type entry struct {
		status, name, fingerprint, severity, startsAt, endsAt, message string
		labels, annotations                                            map[string]string
	}
	entries := make([]entry, 0, 1+len(payload.Alerts))
	entries = append(entries, entry{
		status: payload.Status, name: payload.Name, fingerprint: payload.Fingerprint,
		severity: payload.Severity, startsAt: payload.StartsAt, endsAt: payload.EndsAt,
		labels: payload.Labels, annotations: payload.Annotations, message: payload.Message,
	})
	for _, item := range payload.Alerts {
		entries = append(entries, entry{
			status: item.Status, name: item.Name, fingerprint: item.Fingerprint,
			severity: item.Severity, startsAt: item.StartsAt, endsAt: item.EndsAt,
			labels: item.Labels, annotations: item.Annotations, message: item.Message,
		})
	}
	alerts := make([]Alert, 0, len(entries))
	for _, item := range entries {
		if item.name == "" && len(item.labels) == 0 {
			continue
		}
		alert := Alert{
			Source: SourceGeneric, Sender: string(SourceGeneric), Status: statusOf(item.status, "firing"),
			Name: item.name, Fingerprint: item.fingerprint, Severity: severityOf(item.severity),
			Labels: copyMap(item.labels), Annotations: copyMap(item.annotations), Message: item.message,
		}
		if alert.Name == "" {
			alert.Name = alert.Labels["alertname"]
		}
		alert.StartsAt = parseTime(item.startsAt)
		alert.EndsAt = parseTime(item.endsAt)
		if alert.Fingerprint == "" {
			alert.Fingerprint = stableFingerprint(alert)
		}
		alerts = append(alerts, alert)
	}
	if len(alerts) == 0 {
		return nil, BadRequest("the payload carries no alert")
	}
	return alerts, nil
}

// statusOf normalizes a status string, falling back to the envelope's status.
func statusOf(status, fallback string) Status {
	switch strings.ToLower(firstNonEmpty(status, fallback)) {
	case "resolved", "ok", "recovered", "normal", "okay":
		return StatusResolved
	default:
		return StatusFiring
	}
}

// severityOf maps a sender severity label to the Page vocabulary.
func severityOf(value string) v1alpha1.Severity {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical", "fatal", "page", "p1", "p2", "emergency", "alert":
		return v1alpha1.SeverityCritical
	case "warning", "warn", "p3", "error":
		return v1alpha1.SeverityWarning
	default:
		return v1alpha1.SeverityInfo
	}
}

// datadogStatus maps a Datadog alert status and type to a status.
func datadogStatus(status, alertType string) Status {
	switch strings.ToLower(firstNonEmpty(status, alertType)) {
	case "recovered", "ok", "success":
		return StatusResolved
	default:
		return StatusFiring
	}
}

// datadogSeverity maps a Datadog priority and alert type to a severity.
func datadogSeverity(priority, alertType string) v1alpha1.Severity {
	if severity := severityOf(priority); severity != v1alpha1.SeverityInfo {
		return severity
	}
	return severityOf(alertType)
}

// parseDatadogTags converts Datadog's comma-separated tags to labels.
func parseDatadogTags(tags string) map[string]string {
	labels := make(map[string]string)
	for tag := range strings.SplitSeq(tags, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		key, value, found := strings.Cut(tag, ":")
		if !found {
			labels[tag] = "true"
			continue
		}
		if key == "kube_namespace" || key == "namespace" {
			labels[subject.LabelNamespace] = value
			continue
		}
		if key == "kube_deployment" {
			labels[subject.LabelDeployment] = value
			continue
		}
		if key == "kube_stateful_set" {
			labels[subject.LabelStatefulSet] = value
			continue
		}
		if key == "kube_daemon_set" {
			labels[subject.LabelDaemonSet] = value
			continue
		}
		if key == "pod_name" || key == "kube_pod_name" {
			labels[subject.LabelPod] = value
			continue
		}
		labels[key] = value
	}
	return labels
}

// stableFingerprint builds a deterministic identity for an alert that carries
// none. It excludes timestamps, so a sender's retry produces the same key.
func stableFingerprint(alert Alert) string {
	keys := make([]string, 0, len(alert.Labels))
	for key := range alert.Labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	digest := sha256.New()
	for _, field := range []string{string(alert.Source), alert.Name} {
		digest.Write([]byte(field))
		digest.Write([]byte{0})
	}
	for _, key := range keys {
		digest.Write([]byte(key))
		digest.Write([]byte{'='})
		digest.Write([]byte(alert.Labels[key]))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// parseTime reads an RFC 3339 timestamp, returning the zero time when the
// sender omits or malforms it: a missing time must not invalidate an alert.
func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// copyMap returns a copy, so a decoder cannot mutate the request.
func copyMap(source map[string]string) map[string]string {
	if source == nil {
		return map[string]string{}
	}
	copied := make(map[string]string, len(source))
	maps.Copy(copied, source)
	return copied
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

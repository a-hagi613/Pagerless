// Package contract defines the read-only vocabulary that every Connector
// implements. A Connector is an optional, read-only view of an observability
// backend: its interface has query methods only, so no adapter can express a
// mutation, a deletion, an update, or an administrative call (ADR-0040).
package contract

import (
	"context"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Query asks a log backend for the logs of one Target.
type Query struct {
	// Namespace is the Target's namespace. The server fills it, never the model.
	Namespace string
	// Subject is the Target the logs belong to.
	Subject v1alpha1.TargetReference
	// Pod narrows the query to one Pod when it is set.
	Pod string
	// Container narrows the query to one container when it is set.
	Container string
	// Since is the start of the look-back window.
	Since time.Time
	// Until is the end of the look-back window.
	Until time.Time
	// Limit is the maximum number of entries the backend may return.
	Limit int
	// Filter is an adapter-specific, adapter-validated narrowing expression.
	Filter string
}

// LogEntry is one log line.
type LogEntry struct {
	// Timestamp is when the line was written, when the backend reports it.
	Timestamp time.Time
	// Stream carries the backend's identity labels, such as pod and container.
	Stream map[string]string
	// Line is the log text.
	Line string
	// Severity is the backend's severity label, when it has one.
	Severity string
}

// LogPage is one bounded page of log entries.
type LogPage struct {
	// Entries are the lines, in the backend's order.
	Entries []LogEntry
	// NextToken is the backend's continuation token, when it has one.
	NextToken string
	// Truncated reports whether the backend or the adapter capped the page.
	Truncated bool
	// Bytes is the size of the serialized entries.
	Bytes int
	// Notes record degradation that is part of the evidence, such as a deleted Pod.
	Notes []string
}

// LogConnector is a read-only log source.
type LogConnector interface {
	// Name is the stable connector name used in evidence and metrics.
	Name() string
	// QueryLogs returns one bounded page of logs.
	QueryLogs(ctx context.Context, q Query) (LogPage, error)
}

// MetricQuery asks a metrics backend for one or more series.
type MetricQuery struct {
	// Namespace is the Target's namespace, injected by the server.
	Namespace string
	// Subject is the Target the series describe.
	Subject v1alpha1.TargetReference
	// Query is the backend-native query, already rendered from a validated template.
	Query string
	// Since is the start of the range.
	Since time.Time
	// Until is the end of the range.
	Until time.Time
	// Step is the resolution of the returned series.
	Step time.Duration
	// Limit is the maximum number of series.
	Limit int
}

// MetricPoint is one sample.
type MetricPoint struct {
	// Timestamp is when the sample was taken.
	Timestamp time.Time
	// Value is the sample value.
	Value float64
}

// MetricSeries is one labelled series.
type MetricSeries struct {
	// Labels identify the series.
	Labels map[string]string
	// Points are the samples, oldest first.
	Points []MetricPoint
}

// MetricsConnector is a read-only metrics source.
type MetricsConnector interface {
	// Name is the stable connector name used in evidence and metrics.
	Name() string
	// QueryMetrics returns the series that match the query.
	QueryMetrics(ctx context.Context, q MetricQuery) ([]MetricSeries, error)
}

// AlertQuery asks an alert-state backend which alerts are firing for a Target.
type AlertQuery struct {
	// Namespace is the Target's namespace, injected by the server.
	Namespace string
	// Subject is the Target the alerts describe.
	Subject v1alpha1.TargetReference
	// Since is the start of the range.
	Since time.Time
}

// AlertState is one alert as the backend reports it.
type AlertState struct {
	// Name is the alert name.
	Name string
	// State is the backend's state, such as active or suppressed.
	State string
	// Fingerprint is the backend's alert fingerprint.
	Fingerprint string
	// Labels identify the alert.
	Labels map[string]string
	// StartsAt is when the alert started firing.
	StartsAt time.Time
	// EndsAt is when the alert resolved, when it has resolved.
	EndsAt *time.Time
}

// AlertStateConnector is a read-only alert-state source.
type AlertStateConnector interface {
	// Name is the stable connector name used in evidence and metrics.
	Name() string
	// QueryAlerts returns the alerts that match the query.
	QueryAlerts(ctx context.Context, q AlertQuery) ([]AlertState, error)
}

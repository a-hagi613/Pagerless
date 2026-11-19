package tools

import (
	"context"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// LogQuery asks a Connector for the Target's logs. The broker fills Namespace
// and Subject from the Page, never from the model's arguments.
type LogQuery struct {
	// Namespace is the Page's namespace.
	Namespace string
	// Subject is the Target.
	Subject v1alpha1.TargetReference
	// Pod narrows the query to one of the Target's pods.
	Pod string
	// Container narrows the query to one container.
	Container string
	// Filter is an adapter-validated narrowing expression.
	Filter string
	// Since is the start of the look-back window.
	Since time.Time
	// Until is the end of the window.
	Until time.Time
	// Limit is the maximum number of entries.
	Limit int
}

// LogEntry is one log line from a Connector.
type LogEntry struct {
	// Timestamp is when the line was written.
	Timestamp time.Time
	// Stream carries the backend's labels.
	Stream map[string]string
	// Line is the log text.
	Line string
	// Severity is the backend's severity label.
	Severity string
}

// LogPage is one bounded page of Connector logs.
type LogPage struct {
	// Entries are the lines.
	Entries []LogEntry
	// Truncated reports that a bound cut the page.
	Truncated bool
	// Notes record degradation that is part of the evidence.
	Notes []string
}

// MetricQuery asks a Connector for the Target's series.
type MetricQuery struct {
	// Namespace is the Page's namespace.
	Namespace string
	// Subject is the Target.
	Subject v1alpha1.TargetReference
	// Query is the backend-native query, already bounded by the Connector.
	Query string
	// Since is the start of the range.
	Since time.Time
	// Until is the end of the range.
	Until time.Time
	// Step is the resolution.
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

// ConnectorHealth is what a Connector reports about itself.
type ConnectorHealth struct {
	// Name is the configured Connector name.
	Name string `json:"name"`
	// Kind is its type, such as loki or prometheus.
	Kind string `json:"kind"`
	// Enabled reports whether it is configured.
	Enabled bool `json:"enabled"`
	// Failures is the recent failure count.
	Failures int `json:"failures,omitempty"`
	// LastError is the last error, without a credential.
	LastError string `json:"lastError,omitempty"`
}

// ConnectorSource is the read-only Connector catalogue the broker queries. The
// production implementation wraps internal/connectors; the interface exists so
// that the broker's scope enforcement is tested without an observability
// backend.
//
// No method takes a tenant or a credential: both are fixed in configuration.
type ConnectorSource interface {
	// Names returns the configured Connector names in catalogue order.
	Names() []string
	// Logs queries one log Connector.
	Logs(ctx context.Context, connector string, query LogQuery) (LogPage, error)
	// Metrics queries one metrics Connector.
	Metrics(ctx context.Context, connector string, query MetricQuery) ([]MetricSeries, error)
	// Health reports one Connector's health.
	Health(ctx context.Context, connector string) (ConnectorHealth, error)
}

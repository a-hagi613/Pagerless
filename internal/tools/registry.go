package tools

import (
	"context"
	"encoding/json"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/connectors"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/history"
)

// RegistryConnectors exposes the configured Connector catalogue to the broker.
// It is a thin adapter: the registry owns the backends, the credentials, the
// tenant, and the bounds, and the broker only supplies the Page's namespace and
// Target, which the model cannot change.
type RegistryConnectors struct {
	// Registry is the built Connector catalogue.
	Registry *connectors.Registry
}

// Names returns the configured external Connector names in catalogue order. The
// native Kubernetes collector is not listed: pods_logs is its tool.
func (r RegistryConnectors) Names() []string {
	if r.Registry == nil {
		return nil
	}
	names := make([]string, 0, len(r.Registry.Logs)+len(r.Registry.Metrics)+len(r.Registry.AlertStates))
	for _, connector := range r.Registry.Logs {
		names = append(names, connector.Name())
	}
	for _, connector := range r.Registry.Metrics {
		names = append(names, connector.Name())
	}
	for _, connector := range r.Registry.AlertStates {
		names = append(names, connector.Name())
	}
	return names
}

// Logs queries one log Connector.
func (r RegistryConnectors) Logs(ctx context.Context, connector string, query LogQuery) (LogPage, error) {
	for _, candidate := range r.registry().Logs {
		if candidate.Name() != connector {
			continue
		}
		page, err := candidate.QueryLogs(ctx, contract.Query{
			Namespace: query.Namespace, Subject: query.Subject, Pod: query.Pod,
			Container: query.Container, Since: query.Since, Until: query.Until,
			Limit: query.Limit, Filter: query.Filter,
		})
		if err != nil {
			return LogPage{}, err
		}
		result := LogPage{Truncated: page.Truncated, Notes: page.Notes}
		result.Entries = make([]LogEntry, 0, len(page.Entries))
		for _, entry := range page.Entries {
			result.Entries = append(result.Entries, LogEntry{
				Timestamp: entry.Timestamp, Stream: entry.Stream, Line: entry.Line, Severity: entry.Severity,
			})
		}
		return result, nil
	}
	return LogPage{}, errUnknownConnector(connector)
}

// Metrics queries one metrics Connector.
func (r RegistryConnectors) Metrics(ctx context.Context, connector string, query MetricQuery) ([]MetricSeries, error) {
	for _, candidate := range r.registry().Metrics {
		if candidate.Name() != connector {
			continue
		}
		series, err := candidate.QueryMetrics(ctx, contract.MetricQuery{
			Namespace: query.Namespace, Subject: query.Subject, Query: query.Query,
			Since: query.Since, Until: query.Until, Step: query.Step, Limit: query.Limit,
		})
		if err != nil {
			return nil, err
		}
		result := make([]MetricSeries, 0, len(series))
		for _, item := range series {
			converted := MetricSeries{Labels: item.Labels, Points: make([]MetricPoint, 0, len(item.Points))}
			for _, point := range item.Points {
				converted.Points = append(converted.Points, MetricPoint{Timestamp: point.Timestamp, Value: point.Value})
			}
			result = append(result, converted)
		}
		return result, nil
	}
	return nil, errUnknownConnector(connector)
}

// Health reports one Connector's health. A configured Connector is enabled; its
// recent failure count is the registry's own metric, which is not part of this
// build's contract, so the report states that it is healthy only when it is
// configured rather than inventing a measurement.
func (r RegistryConnectors) Health(_ context.Context, connector string) (ConnectorHealth, error) {
	for _, candidate := range r.registry().Logs {
		if candidate.Name() == connector {
			return ConnectorHealth{Name: connector, Kind: "logs", Enabled: true}, nil
		}
	}
	for _, candidate := range r.registry().Metrics {
		if candidate.Name() == connector {
			return ConnectorHealth{Name: connector, Kind: "metrics", Enabled: true}, nil
		}
	}
	for _, candidate := range r.registry().AlertStates {
		if candidate.Name() == connector {
			return ConnectorHealth{Name: connector, Kind: "alert-state", Enabled: true}, nil
		}
	}
	return ConnectorHealth{}, errUnknownConnector(connector)
}

// registry returns the registry or an empty one.
func (r RegistryConnectors) registry() *connectors.Registry {
	if r.Registry == nil {
		return &connectors.Registry{}
	}
	return r.Registry
}

// errUnknownConnector is returned for a name outside the configured catalogue.
type errUnknownConnector string

func (e errUnknownConnector) Error() string {
	return "connector " + string(e) + " is not configured"
}

// HistoryContext serves the Context Bundle that was assembled before the
// investigation started. It reads the stored body, so the agent and the Portal
// see exactly the same evidence.
type HistoryContext struct {
	// Store is the history database that holds the bundle.
	Store *history.Store
}

// Context returns the stored bundle as JSON. A Page with no bundle reports an
// empty object rather than failing the manifest.
func (c *HistoryContext) Context(ctx context.Context, page *v1alpha1.Page) (json.RawMessage, error) {
	if c.Store == nil {
		return []byte(`{}`), nil
	}
	bundle, found, err := c.Store.Bundle(ctx, string(page.UID))
	if err != nil {
		return nil, err
	}
	if !found || bundle.Body == "" {
		return []byte(`{}`), nil
	}
	return []byte(bundle.Body), nil
}

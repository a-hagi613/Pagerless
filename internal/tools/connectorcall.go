package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	// defaultConnectorLogs bounds one Connector log query.
	defaultConnectorLogs = 200
	// maxConnectorLogs is the largest log page a caller may ask for.
	maxConnectorLogs = 1000
	// defaultConnectorSeries bounds one metrics query.
	defaultConnectorSeries = 20
	// maxConnectorSeries is the largest series count a caller may ask for.
	maxConnectorSeries = 50
	// defaultMetricStep is the default resolution.
	defaultMetricStep = 60
)

// connectorArguments are the arguments the Connector tools share. There is
// deliberately no namespace and no tenant: the broker fills both from the Page
// and the Connector configuration.
type connectorArguments struct {
	Connector    string `json:"connector"`
	Pod          string `json:"pod"`
	Container    string `json:"container"`
	Filter       string `json:"filter"`
	Query        string `json:"query"`
	SinceSeconds int64  `json:"sinceSeconds"`
	StepSeconds  int64  `json:"stepSeconds"`
	Limit        int    `json:"limit"`
}

// connector runs one Connector read. The namespace and Target selector are
// always the Page's, whatever the model's query says.
func (b *Broker) connector(
	ctx context.Context, page *v1alpha1.Page, scope Scope, tool string, raw json.RawMessage,
) (any, error) {
	var args connectorArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}
	if err := checkConnector(scope, args.Connector); err != nil {
		return nil, err
	}
	if b.Connectors == nil {
		return nil, fmt.Errorf("no Connector is configured, so %s cannot answer", tool)
	}
	now := b.now()
	since := args.SinceSeconds
	if since <= 0 {
		since = defaultSinceSeconds
	}
	from := now.Add(-time.Duration(since) * time.Second)
	switch tool {
	case ToolConnectorLogs:
		limit := args.Limit
		if limit <= 0 {
			limit = defaultConnectorLogs
		}
		if limit > maxConnectorLogs {
			limit = maxConnectorLogs
		}
		if args.Pod != "" {
			pods, err := targetPodNames(ctx, b.Reader, page.Spec.Target)
			if err != nil {
				return nil, err
			}
			if _, ok := pods[args.Pod]; !ok {
				return nil, fmt.Errorf("%w: pod %s is not part of the Target", ErrOutOfScope, args.Pod)
			}
		}
		return b.Connectors.Logs(ctx, args.Connector, LogQuery{
			Namespace: page.Spec.Target.Namespace,
			Subject:   page.Spec.Target,
			Pod:       args.Pod,
			Container: args.Container,
			Filter:    args.Filter,
			Since:     from,
			Until:     now,
			Limit:     limit,
		})
	case ToolConnectorMetrics:
		limit := args.Limit
		if limit <= 0 {
			limit = defaultConnectorSeries
		}
		if limit > maxConnectorSeries {
			limit = maxConnectorSeries
		}
		step := args.StepSeconds
		if step <= 0 {
			step = defaultMetricStep
		}
		return b.Connectors.Metrics(ctx, args.Connector, MetricQuery{
			Namespace: page.Spec.Target.Namespace,
			Subject:   page.Spec.Target,
			Query:     args.Query,
			Since:     from,
			Until:     now,
			Step:      time.Duration(step) * time.Second,
			Limit:     limit,
		})
	case ToolConnectorHealth:
		return b.Connectors.Health(ctx, args.Connector)
	default:
		return nil, fmt.Errorf("%w: the tool %s is not a Connector read", ErrOutOfScope, tool)
	}
}

// checkConnector refuses a Connector outside the Page's configured set.
func checkConnector(scope Scope, connector string) error {
	if connector == "" {
		return fmt.Errorf("%w: connector is required", ErrInvalidArguments)
	}
	if slices.Contains(scope.Connectors, connector) {
		return nil
	}
	return fmt.Errorf("%w: connector %s is not configured for this installation", ErrOutOfScope, connector)
}

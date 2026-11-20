package connectors

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/connectors/alertstate"
	"github.com/a-hagi613/pagerless/internal/connectors/aws"
	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/connectors/contract"
	"github.com/a-hagi613/pagerless/internal/connectors/logs"
	"github.com/a-hagi613/pagerless/internal/connectors/mcp"
	"github.com/a-hagi613/pagerless/internal/connectors/metrics"
)

// Deps are the collaborators the registry builds Connectors with.
type Deps struct {
	// Reader reads Pods and their owners for the native collector.
	Reader client.Reader
	// PodLogs reads the pod log subresource.
	PodLogs logs.PodLogReader
	// Credentials resolves Secret references.
	Credentials base.Credentials
	// Metrics records Connector health. A nil value builds a private registry.
	Metrics *base.Metrics
	// AllowedHosts, when non-empty, is the only host set Connectors may reach.
	AllowedHosts []string
	// Now defaults to time.Now.
	Now func() time.Time
	// Version is the client version reported by the MCP bridge.
	Version string
}

// Registry is the built Connector catalogue. The slices are in the fixed
// assembly order the Context Bundle uses.
type Registry struct {
	// Native is the mandatory Kubernetes log collector.
	Native contract.LogConnector
	// Logs are the enabled external log Connectors, in catalogue order.
	Logs []contract.LogConnector
	// Metrics are the enabled metrics Connectors, in catalogue order.
	Metrics []contract.MetricsConnector
	// AlertStates are the enabled alert-state Connectors, in catalogue order.
	AlertStates []contract.AlertStateConnector
	// Redaction is the compiled operator pattern set.
	Redaction []*regexp.Regexp
	// NativeConfig is the native collector's bounds.
	NativeConfig NativeConfig
}

// NewRegistry validates the configuration and builds every enabled Connector.
// A configuration that names an unreachable Secret, a malformed query
// template, or an invalid URL fails here, at startup.
func NewRegistry(cfg Config, deps Deps) (*Registry, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.Metrics == nil {
		recorder, err := base.NewMetrics(nil)
		if err != nil {
			return nil, fmt.Errorf("create the Connector metrics: %w", err)
		}
		deps.Metrics = recorder
	}
	ctx := context.Background()
	registry := &Registry{NativeConfig: cfg.Logs.NativeKubernetes}
	pattern, err := compileRedactions(cfg.Redaction.Patterns)
	if err != nil {
		return nil, err
	}
	registry.Redaction = pattern
	registry.Native = logs.NewNative(deps.Reader, deps.PodLogs,
		cfg.Logs.NativeKubernetes.TailLines, cfg.Logs.NativeKubernetes.LimitBytes,
		cfg.Logs.NativeKubernetes.SinceSeconds, cfg.Logs.NativeKubernetes.MaxPods,
		cfg.Logs.NativeKubernetes.MaxContainersPerPod, cfg.Logs.NativeKubernetes.Concurrency, nil)
	if err := registry.buildLogs(ctx, cfg, deps); err != nil {
		return nil, err
	}
	if err := registry.buildMetrics(ctx, cfg, deps); err != nil {
		return nil, err
	}
	if err := registry.buildAlertStates(ctx, cfg, deps); err != nil {
		return nil, err
	}
	return registry, nil
}

// compileRedactions compiles the operator patterns.
func compileRedactions(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		expression, err := base.CompileRedaction(pattern)
		if err != nil {
			return nil, fmt.Errorf("connectors.redaction.patterns: %w", err)
		}
		compiled = append(compiled, expression)
	}
	return compiled, nil
}

// newClient builds one guarded client.
func newClient(name, baseURL string, auth Auth, policy guard, deps Deps) (*base.Client, error) {
	return base.NewClient(base.ClientOptions{
		Name: name, BaseURL: baseURL, Auth: auth, Policy: policy.policy(),
		Credentials: deps.Credentials, AllowedHosts: deps.AllowedHosts, Metrics: deps.Metrics, Now: deps.Now,
	})
}

// buildLogs builds the external log Connectors.
func (r *Registry) buildLogs(ctx context.Context, cfg Config, deps Deps) error {
	if cfg.Logs.Loki.Enabled {
		httpClient, err := newClient("loki", cfg.Logs.Loki.URL, cfg.Logs.Loki.Auth, cfg.Logs.Loki.guard(), deps)
		if err != nil {
			return err
		}
		template, err := base.CompileTemplate("connectors.logs.loki.queryTemplate", cfg.Logs.Loki.QueryTemplate, true)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Logs.Loki.Auth, deps.Credentials); err != nil {
			return err
		}
		r.Logs = append(r.Logs, logs.NewLoki(httpClient, template, cfg.Logs.Loki.TenantID, cfg.Logs.Loki.Limit))
	}
	if cfg.Logs.Grafana.Enabled {
		httpClient, err := newClient("grafana", cfg.Logs.Grafana.URL, cfg.Logs.Grafana.Auth, cfg.Logs.Grafana.guard(), deps)
		if err != nil {
			return err
		}
		template, err := base.CompileTemplate("connectors.logs.grafana.queryTemplate", cfg.Logs.Grafana.QueryTemplate, true)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Logs.Grafana.Auth, deps.Credentials); err != nil {
			return err
		}
		r.Logs = append(r.Logs, logs.NewGrafana(httpClient, cfg.Logs.Grafana.Mode, cfg.Logs.Grafana.DatasourceUID,
			template, cfg.Logs.Grafana.Limit, cfg.ClusterName, cfg.Environment))
	}
	if cfg.Logs.CloudWatchLogs.Enabled {
		httpClient, err := newClient("cloudwatch-logs", "", cfg.Logs.CloudWatchLogs.Auth,
			cfg.Logs.CloudWatchLogs.guard(), deps)
		if err != nil {
			return err
		}
		provider, err := awsProvider(ctx, cfg.Logs.CloudWatchLogs.Auth, deps.Credentials)
		if err != nil {
			return err
		}
		template, err := base.CompileTemplate("connectors.logs.cloudwatchLogs.logGroupNameTemplate",
			cfg.Logs.CloudWatchLogs.LogGroupNameTemplate, true)
		if err != nil {
			return err
		}
		region := cfg.Logs.CloudWatchLogs.Region
		caller := &aws.Caller{Doer: httpClient, Provider: provider, Region: region, Service: "logs",
			Endpoint: "https://logs." + region + ".amazonaws.com/", Now: deps.Now}
		r.Logs = append(r.Logs, logs.NewCloudWatch(httpClient, caller, region, template,
			cfg.Logs.CloudWatchLogs.LogStreamNamePrefix, cfg.Logs.CloudWatchLogs.FilterPattern,
			cfg.Logs.CloudWatchLogs.Mode, cfg.Logs.CloudWatchLogs.Limit, cfg.ClusterName, cfg.Environment))
	}
	if cfg.Logs.DatadogLogs.Enabled {
		httpClient, err := newClient("datadog-logs", "https://api."+cfg.Logs.DatadogLogs.Site, cfg.Logs.DatadogLogs.Auth,
			cfg.Logs.DatadogLogs.guard(), deps)
		if err != nil {
			return err
		}
		template, err := base.CompileTemplate("connectors.logs.datadogLogs.queryTemplate",
			cfg.Logs.DatadogLogs.QueryTemplate, true)
		if err != nil {
			return err
		}
		apiKey, err := requiredCredential(ctx, cfg.Logs.DatadogLogs.Auth,
			cfg.Logs.DatadogLogs.Auth.APIKeyKey, deps.Credentials)
		if err != nil {
			return err
		}
		appKey, err := requiredCredential(ctx, cfg.Logs.DatadogLogs.Auth,
			cfg.Logs.DatadogLogs.Auth.AppKeyKey, deps.Credentials)
		if err != nil {
			return err
		}
		r.Logs = append(r.Logs, logs.NewDatadog(httpClient, template, cfg.Logs.DatadogLogs.Limit, apiKey, appKey,
			cfg.ClusterName, cfg.Environment))
	}
	if cfg.Logs.OpenSearch.Enabled {
		httpClient, err := newClient("opensearch", cfg.Logs.OpenSearch.URL, cfg.Logs.OpenSearch.Auth,
			cfg.Logs.OpenSearch.guard(), deps)
		if err != nil {
			return err
		}
		index, err := base.CompileTemplate("connectors.logs.opensearch.indexTemplate",
			cfg.Logs.OpenSearch.IndexTemplate, true)
		if err != nil {
			return err
		}
		query, err := base.CompileTemplate("connectors.logs.opensearch.queryTemplate",
			cfg.Logs.OpenSearch.QueryTemplate, false)
		if err != nil {
			return err
		}
		apiKey, err := optionalCredential(ctx, cfg.Logs.OpenSearch.Auth, cfg.Logs.OpenSearch.Auth.APIKeyKey, deps.Credentials)
		if err != nil {
			return err
		}
		r.Logs = append(r.Logs, logs.NewOpenSearch(httpClient, index, query, cfg.Logs.OpenSearch.Limit, apiKey,
			cfg.ClusterName, cfg.Environment))
	}
	if cfg.Logs.MCP.Enabled {
		httpClient, err := newClient("mcp-logs", cfg.Logs.MCP.URL, cfg.Logs.MCP.Auth, cfg.Logs.MCP.guard(), deps)
		if err != nil {
			return err
		}
		arguments, err := base.CompileTemplate("connectors.logs.mcp.argumentsTemplate",
			cfg.Logs.MCP.ArgumentsTemplate, true)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Logs.MCP.Auth, deps.Credentials); err != nil {
			return err
		}
		bridge := &mcp.Client{Doer: httpClient, URL: cfg.Logs.MCP.URL, Tool: cfg.Logs.MCP.Tool, Version: deps.Version}
		r.Logs = append(r.Logs, logs.NewMCP(bridge, arguments, cfg.Logs.MCP.MaxBytes,
			cfg.ClusterName, cfg.Environment))
	}
	return nil
}

// buildMetrics builds the metrics Connectors.
func (r *Registry) buildMetrics(ctx context.Context, cfg Config, deps Deps) error {
	if cfg.Metrics.Prometheus.Enabled {
		httpClient, err := newClient("prometheus", cfg.Metrics.Prometheus.URL, cfg.Metrics.Prometheus.Auth,
			cfg.Metrics.Prometheus.guard(), deps)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Metrics.Prometheus.Auth, deps.Credentials); err != nil {
			return err
		}
		queries, err := compileQueries("connectors.metrics.prometheus.queries", cfg.Metrics.Prometheus.Queries)
		if err != nil {
			return err
		}
		r.Metrics = append(r.Metrics, metrics.NewPrometheus(httpClient, queries, cfg.Metrics.Prometheus.Step.Std(),
			cfg.Metrics.Prometheus.MaxSamples, cfg.ClusterName, cfg.Environment))
	}
	if cfg.Metrics.Grafana.Enabled {
		if cfg.Metrics.Grafana.QueryTemplate == "" {
			return errors.New("connectors.metrics.grafana.queryTemplate is required when the Connector is enabled")
		}
		httpClient, err := newClient("grafana-metrics", cfg.Metrics.Grafana.URL, cfg.Metrics.Grafana.Auth,
			cfg.Metrics.Grafana.guard(), deps)
		if err != nil {
			return err
		}
		template, err := base.CompileTemplate("connectors.metrics.grafana.queryTemplate",
			cfg.Metrics.Grafana.QueryTemplate, true)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Metrics.Grafana.Auth, deps.Credentials); err != nil {
			return err
		}
		r.Metrics = append(r.Metrics, metrics.NewGrafana(httpClient, cfg.Metrics.Grafana.Mode,
			cfg.Metrics.Grafana.DatasourceUID, template, cfg.Metrics.Grafana.Step.Std(),
			cfg.Metrics.Grafana.Limit, cfg.ClusterName, cfg.Environment))
	}
	if cfg.Metrics.CloudWatch.Enabled {
		httpClient, err := newClient("cloudwatch-metrics", "", cfg.Metrics.CloudWatch.Auth,
			cfg.Metrics.CloudWatch.guard(), deps)
		if err != nil {
			return err
		}
		provider, err := awsProvider(ctx, cfg.Metrics.CloudWatch.Auth, deps.Credentials)
		if err != nil {
			return err
		}
		queries, err := compileQueries("connectors.metrics.cloudwatch.queries", cfg.Metrics.CloudWatch.Queries)
		if err != nil {
			return err
		}
		region := cfg.Metrics.CloudWatch.Region
		caller := &aws.Caller{Doer: httpClient, Provider: provider, Region: region, Service: "monitoring",
			Endpoint: "https://monitoring." + region + ".amazonaws.com/", Now: deps.Now}
		r.Metrics = append(r.Metrics, metrics.NewCloudWatch(httpClient, caller, cfg.Metrics.CloudWatch.Namespace,
			queries, cfg.ClusterName, cfg.Environment))
	}
	if cfg.Metrics.Datadog.Enabled {
		httpClient, err := newClient("datadog-metrics", "https://api."+cfg.Metrics.Datadog.Site, cfg.Metrics.Datadog.Auth,
			cfg.Metrics.Datadog.guard(), deps)
		if err != nil {
			return err
		}
		queries, err := compileQueries("connectors.metrics.datadog.queries", cfg.Metrics.Datadog.Queries)
		if err != nil {
			return err
		}
		apiKey, err := requiredCredential(ctx, cfg.Metrics.Datadog.Auth, cfg.Metrics.Datadog.Auth.APIKeyKey, deps.Credentials)
		if err != nil {
			return err
		}
		appKey, err := requiredCredential(ctx, cfg.Metrics.Datadog.Auth, cfg.Metrics.Datadog.Auth.AppKeyKey, deps.Credentials)
		if err != nil {
			return err
		}
		r.Metrics = append(r.Metrics, metrics.NewDatadog(httpClient, queries, apiKey, appKey,
			cfg.ClusterName, cfg.Environment))
	}
	if cfg.Metrics.MCP.Enabled {
		httpClient, err := newClient("mcp-metrics", cfg.Metrics.MCP.URL, cfg.Metrics.MCP.Auth, cfg.Metrics.MCP.guard(), deps)
		if err != nil {
			return err
		}
		arguments, err := base.CompileTemplate("connectors.metrics.mcp.argumentsTemplate",
			cfg.Metrics.MCP.ArgumentsTemplate, true)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.Metrics.MCP.Auth, deps.Credentials); err != nil {
			return err
		}
		bridge := &mcp.Client{Doer: httpClient, URL: cfg.Metrics.MCP.URL, Tool: cfg.Metrics.MCP.Tool, Version: deps.Version}
		r.Metrics = append(r.Metrics, metrics.NewMCP(bridge, arguments, cfg.ClusterName, cfg.Environment))
	}
	return nil
}

// buildAlertStates builds the alert-state Connectors.
func (r *Registry) buildAlertStates(ctx context.Context, cfg Config, deps Deps) error {
	if cfg.AlertState.Alertmanager.Enabled {
		httpClient, err := newClient("alertmanager", cfg.AlertState.Alertmanager.URL, cfg.AlertState.Alertmanager.Auth,
			cfg.AlertState.Alertmanager.guard(), deps)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.AlertState.Alertmanager.Auth, deps.Credentials); err != nil {
			return err
		}
		r.AlertStates = append(r.AlertStates,
			alertstate.NewAlertmanager(httpClient, cfg.AlertState.Alertmanager.FilterByNamespace))
	}
	if cfg.AlertState.Grafana.Enabled {
		httpClient, err := newClient("grafana-alertstate", cfg.AlertState.Grafana.URL, cfg.AlertState.Grafana.Auth,
			cfg.AlertState.Grafana.guard(), deps)
		if err != nil {
			return err
		}
		if err := requireCredentials(ctx, cfg.AlertState.Grafana.Auth, deps.Credentials); err != nil {
			return err
		}
		r.AlertStates = append(r.AlertStates, alertstate.NewGrafana(httpClient))
	}
	return nil
}

// compileQueries compiles a named query template map.
func compileQueries(prefix string, queries map[string]string) (map[string]*base.Template, error) {
	compiled := make(map[string]*base.Template, len(queries))
	for name, query := range queries {
		template, err := base.CompileTemplate(prefix+"."+name, query, true)
		if err != nil {
			return nil, err
		}
		compiled[name] = template
	}
	return compiled, nil
}

// awsProvider resolves the AWS credential source for a Connector.
func awsProvider(ctx context.Context, auth Auth, credentials base.Credentials) (aws.Provider, error) {
	if auth.Mode == authModeStatic {
		accessKey, err := requiredCredential(ctx, auth, auth.AccessKeyIDKey, credentials)
		if err != nil {
			return nil, err
		}
		secret, err := requiredCredential(ctx, auth, auth.SecretAccessKeyKey, credentials)
		if err != nil {
			return nil, err
		}
		return aws.Static{Value: aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secret}}, nil
	}
	return aws.Environment{}, nil
}

// requireCredentials checks every configured credential key resolves.
func requireCredentials(ctx context.Context, auth Auth, credentials base.Credentials) error {
	for _, key := range []string{auth.TokenKey, auth.UsernameKey, auth.PasswordKey, auth.APIKeyKey, auth.AppKeyKey,
		auth.AccessKeyIDKey, auth.SecretAccessKeyKey} {
		if key == "" {
			continue
		}
		if _, err := requiredCredential(ctx, auth, key, credentials); err != nil {
			return err
		}
	}
	return nil
}

// requiredCredential resolves one key and fails when it is not mounted.
func requiredCredential(
	ctx context.Context, auth Auth, key string, credentials base.Credentials,
) (string, error) {
	value, err := credentials.Get(ctx, auth.SecretName, key)
	if err != nil {
		return "", fmt.Errorf("connector credential %s/%s: %w", auth.SecretName, key, err)
	}
	if value == "" {
		return "", fmt.Errorf("connector credential %s/%s is empty", auth.SecretName, key)
	}
	return value, nil
}

// optionalCredential resolves one key and returns empty when it is not mounted.
func optionalCredential(
	ctx context.Context, auth Auth, key string, credentials base.Credentials,
) (string, error) {
	if key == "" {
		return "", nil
	}
	value, err := credentials.Get(ctx, auth.SecretName, key)
	if err != nil {
		if errors.Is(err, base.ErrMissingCredential) {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

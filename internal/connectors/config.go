package connectors

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
	"github.com/a-hagi613/pagerless/internal/subject"
)

// Duration is a time.Duration that decodes from YAML strings such as "15s".
type Duration time.Duration

// UnmarshalYAML decodes a duration from a YAML string.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var text string
	if err := value.Decode(&text); err != nil {
		return fmt.Errorf("a duration must be a string such as \"15s\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML encodes a duration as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Auth modes shared by the AWS Connectors.
const (
	// authModeIRSA uses the service account's instance role.
	authModeIRSA = "irsa"
	// authModeStatic reads an access key and secret from the credentials Secret.
	authModeStatic = "static"
)

// Config is the non-secret half of every Connector. It is mounted from the
// pagerless-connectors ConfigMap. Credentials appear only as Secret references
// (Auth) and never as values.
type Config struct {
	// Logs configures the log Connectors, including the mandatory native collector.
	Logs LogsConfig `yaml:"logs"`
	// Metrics configures the metrics Connectors.
	Metrics MetricsConfig `yaml:"metrics"`
	// AlertState configures the alert-state Connectors.
	AlertState AlertStateConfig `yaml:"alertState"`
	// Subject configures how an inbound alert names its Target.
	Subject SubjectConfig `yaml:"subject,omitempty"`
	// Redaction configures the operator's own sanitization patterns.
	Redaction RedactionConfig `yaml:"redaction"`
	// Alerting configures the inbound webhook receiver.
	Alerting AlertingConfig `yaml:"alerting,omitempty"`
	// AllowedHosts, when non-empty, is the only set of hosts a Connector may reach.
	AllowedHosts []string `yaml:"allowedHosts,omitempty"`
	// ClusterName is interpolated as {{ .ClusterName }} in templates.
	ClusterName string `yaml:"clusterName,omitempty"`
	// Environment is interpolated as {{ .Environment }} in templates.
	Environment string `yaml:"environment,omitempty"`
}

// RedactionConfig is the operator's sanitization configuration.
type RedactionConfig struct {
	// Patterns are regular expressions replaced before a bundle is stored.
	Patterns []string `yaml:"patterns,omitempty"`
}

// SubjectConfig configures subject resolution for inbound alerts.
type SubjectConfig struct {
	// Rules are the extraction rules tried after the explicit labels,
	// workload labels, pod owner chain, and subject annotation.
	Rules []subject.Rule `yaml:"rules,omitempty"`
}

// AlertingConfig configures the inbound webhook receiver's non-secret bounds.
type AlertingConfig struct {
	// SNSTopics is the allowlist of SNS topic ARNs. An empty list refuses
	// every SNS delivery, because an unauthenticated topic is not a sender.
	SNSTopics []string `yaml:"snsTopics,omitempty"`
	// VerifySNSSignature verifies the signature of an SNS delivery.
	VerifySNSSignature bool `yaml:"verifySNSSignature,omitempty"`
	// MaxAlerts is the number of alerts decoded from one request.
	MaxAlerts int `yaml:"maxAlerts,omitempty"`
	// RequestsPerSecond is the per-source rate.
	RequestsPerSecond float64 `yaml:"requestsPerSecond,omitempty"`
	// Burst is the per-source burst allowance.
	Burst int `yaml:"burst,omitempty"`
}

// LogsConfig is the log Connector catalogue.
type LogsConfig struct {
	// NativeKubernetes is mandatory and cannot be disabled.
	NativeKubernetes NativeConfig `yaml:"nativeKubernetes"`
	// Loki queries Grafana Loki.
	Loki LokiConfig `yaml:"loki,omitempty"`
	// Grafana queries a datasource through Grafana.
	Grafana GrafanaConfig `yaml:"grafana,omitempty"`
	// CloudWatchLogs queries AWS CloudWatch Logs.
	CloudWatchLogs CloudWatchLogsConfig `yaml:"cloudwatchLogs,omitempty"`
	// DatadogLogs queries the Datadog Logs API.
	DatadogLogs DatadogLogsConfig `yaml:"datadogLogs,omitempty"`
	// OpenSearch queries an OpenSearch index.
	OpenSearch OpenSearchConfig `yaml:"opensearch,omitempty"`
	// MCP calls one read-only tool on a Streamable HTTP MCP server.
	MCP MCPConfig `yaml:"mcp,omitempty"`
}

// MetricsConfig is the metrics Connector catalogue.
type MetricsConfig struct {
	// Prometheus queries a Prometheus-compatible endpoint.
	Prometheus PrometheusConfig `yaml:"prometheus,omitempty"`
	// Grafana queries a metrics datasource through Grafana.
	Grafana GrafanaConfig `yaml:"grafana,omitempty"`
	// CloudWatch queries AWS CloudWatch metrics.
	CloudWatch CloudWatchMetricsConfig `yaml:"cloudwatch,omitempty"`
	// Datadog queries the Datadog metrics API.
	Datadog DatadogMetricsConfig `yaml:"datadog,omitempty"`
	// MCP calls one read-only tool on a Streamable HTTP MCP server.
	MCP MCPConfig `yaml:"mcp,omitempty"`
}

// AlertStateConfig is the alert-state Connector catalogue.
type AlertStateConfig struct {
	// Alertmanager reads the firing alerts of an Alertmanager.
	Alertmanager AlertmanagerConfig `yaml:"alertmanager,omitempty"`
	// Grafana reads the firing alerts of Grafana's embedded Alertmanager.
	Grafana GrafanaAlertStateConfig `yaml:"grafana,omitempty"`
}

// NativeConfig tunes the mandatory native Kubernetes log collector.
type NativeConfig struct {
	// Enabled must be true. It exists only so the chart can reject false with a
	// clear message rather than silently disabling the only zero-dependency source.
	Enabled bool `yaml:"enabled"`
	// TailLines is the tail requested from the pod log subresource.
	TailLines int64 `yaml:"tailLines,omitempty"`
	// LimitBytes is the byte ceiling of one container's log.
	LimitBytes int64 `yaml:"limitBytes,omitempty"`
	// SinceSeconds is the look-back window, in seconds.
	SinceSeconds int64 `yaml:"sinceSeconds,omitempty"`
	// MaxPods is the number of Pods one bundle reads.
	MaxPods int `yaml:"maxPods,omitempty"`
	// MaxContainersPerPod is the number of containers one bundle reads per Pod.
	MaxContainersPerPod int `yaml:"maxContainersPerPod,omitempty"`
	// Concurrency is the number of pod log reads in flight.
	Concurrency int `yaml:"concurrency,omitempty"`
}

// Auth is a reference to credentials. It never carries a value.
type Auth = base.Auth

// guard is the retry and overload policy every Connector shares.
type guard struct {
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	MaxPages          int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
	FailureThreshold  int      `yaml:"failureThreshold,omitempty"`
	Cooldown          Duration `yaml:"cooldown,omitempty"`
}

// LokiConfig configures the Loki Connector.
type LokiConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	TenantID          string   `yaml:"tenantID,omitempty"`
	QueryTemplate     string   `yaml:"queryTemplate,omitempty"`
	Limit             int      `yaml:"limit,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	MaxPages          int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// GrafanaConfig configures the Grafana Connector used for logs or metrics.
type GrafanaConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	Mode              string   `yaml:"mode,omitempty"`
	DatasourceUID     string   `yaml:"datasourceUID,omitempty"`
	QueryTemplate     string   `yaml:"queryTemplate,omitempty"`
	Limit             int      `yaml:"limit,omitempty"`
	Step              Duration `yaml:"step,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	MaxPages          int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// CloudWatchLogsConfig configures the CloudWatch Logs Connector.
type CloudWatchLogsConfig struct {
	Enabled              bool     `yaml:"enabled"`
	Region               string   `yaml:"region,omitempty"`
	LogGroupNameTemplate string   `yaml:"logGroupNameTemplate,omitempty"`
	LogStreamNamePrefix  string   `yaml:"logStreamNamePrefix,omitempty"`
	FilterPattern        string   `yaml:"filterPattern,omitempty"`
	Mode                 string   `yaml:"mode,omitempty"`
	Limit                int      `yaml:"limit,omitempty"`
	Auth                 Auth     `yaml:"auth,omitempty"`
	Timeout              Duration `yaml:"timeout,omitempty"`
	MaxBytes             int64    `yaml:"maxBytes,omitempty"`
	MaxPages             int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond    float64  `yaml:"requestsPerSecond,omitempty"`
	Burst                int      `yaml:"burst,omitempty"`
}

// DatadogLogsConfig configures the Datadog Logs Connector.
type DatadogLogsConfig struct {
	Enabled           bool     `yaml:"enabled"`
	Site              string   `yaml:"site,omitempty"`
	QueryTemplate     string   `yaml:"queryTemplate,omitempty"`
	Limit             int      `yaml:"limit,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	MaxPages          int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// OpenSearchConfig configures the OpenSearch Connector.
type OpenSearchConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	IndexTemplate     string   `yaml:"indexTemplate,omitempty"`
	QueryTemplate     string   `yaml:"queryTemplate,omitempty"`
	Limit             int      `yaml:"limit,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	MaxPages          int      `yaml:"maxPages,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// MCPConfig configures a Streamable HTTP MCP Connector.
type MCPConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	Tool              string   `yaml:"tool,omitempty"`
	ArgumentsTemplate string   `yaml:"argumentsTemplate,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// PrometheusConfig configures the Prometheus Connector.
type PrometheusConfig struct {
	Enabled           bool              `yaml:"enabled"`
	URL               string            `yaml:"url,omitempty"`
	Step              Duration          `yaml:"step,omitempty"`
	MaxSamples        int               `yaml:"maxSamples,omitempty"`
	Queries           map[string]string `yaml:"queries,omitempty"`
	Auth              Auth              `yaml:"auth,omitempty"`
	Timeout           Duration          `yaml:"timeout,omitempty"`
	MaxBytes          int64             `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64           `yaml:"requestsPerSecond,omitempty"`
	Burst             int               `yaml:"burst,omitempty"`
}

// CloudWatchMetricsConfig configures the CloudWatch metrics Connector.
type CloudWatchMetricsConfig struct {
	Enabled           bool              `yaml:"enabled"`
	Region            string            `yaml:"region,omitempty"`
	Namespace         string            `yaml:"namespace,omitempty"`
	Queries           map[string]string `yaml:"queries,omitempty"`
	Auth              Auth              `yaml:"auth,omitempty"`
	Timeout           Duration          `yaml:"timeout,omitempty"`
	MaxBytes          int64             `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64           `yaml:"requestsPerSecond,omitempty"`
	Burst             int               `yaml:"burst,omitempty"`
}

// DatadogMetricsConfig configures the Datadog metrics Connector.
type DatadogMetricsConfig struct {
	Enabled           bool              `yaml:"enabled"`
	Site              string            `yaml:"site,omitempty"`
	Queries           map[string]string `yaml:"queries,omitempty"`
	Auth              Auth              `yaml:"auth,omitempty"`
	Timeout           Duration          `yaml:"timeout,omitempty"`
	MaxBytes          int64             `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64           `yaml:"requestsPerSecond,omitempty"`
	Burst             int               `yaml:"burst,omitempty"`
}

// AlertmanagerConfig configures the Alertmanager alert-state Connector.
type AlertmanagerConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	FilterByNamespace bool     `yaml:"filterByNamespace,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// GrafanaAlertStateConfig configures the Grafana alert-state Connector.
type GrafanaAlertStateConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url,omitempty"`
	Auth              Auth     `yaml:"auth,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	MaxBytes          int64    `yaml:"maxBytes,omitempty"`
	RequestsPerSecond float64  `yaml:"requestsPerSecond,omitempty"`
	Burst             int      `yaml:"burst,omitempty"`
}

// LoadConfig reads the Connector configuration from path. Unknown fields are
// rejected, which is how a configuration that tries to carry a secret value
// instead of a reference fails the boot rather than starting with a credential
// in a ConfigMap.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read the connectors configuration %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and validates the Connector configuration.
func ParseConfig(raw []byte) (Config, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Config{}, errors.New("the connectors configuration is empty")
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode the connectors configuration: %w", err)
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces the invariants that must hold before the server serves any
// Page. A configuration that would silently disable mandatory evidence, reach
// an invalid URL, or name a placeholder that no template engine substitutes
// fails here.
func (c Config) Validate() error {
	if !c.Logs.NativeKubernetes.Enabled {
		return errors.New("connectors.logs.nativeKubernetes.enabled must be true: " +
			"native Kubernetes logs are the only evidence source that needs no external service")
	}
	if err := c.Logs.NativeKubernetes.validate(); err != nil {
		return err
	}
	validators := []func() error{
		c.Logs.Loki.validate, c.Logs.Grafana.validate, c.Logs.CloudWatchLogs.validate,
		c.Logs.DatadogLogs.validate, c.Logs.OpenSearch.validate, c.Logs.MCP.validate,
		c.Metrics.Prometheus.validate, c.Metrics.Grafana.validate, c.Metrics.CloudWatch.validate,
		c.Metrics.Datadog.validate, c.Metrics.MCP.validate,
		c.AlertState.Alertmanager.validate, c.AlertState.Grafana.validate,
	}
	for _, validate := range validators {
		if err := validate(); err != nil {
			return err
		}
	}
	if err := validateRedaction(c.Redaction); err != nil {
		return err
	}
	for index := range c.Subject.Rules {
		if err := c.Subject.Rules[index].Compile(); err != nil {
			return fmt.Errorf("connectors.subject.rules[%d]: %w", index, err)
		}
	}
	switch {
	case c.Alerting.MaxAlerts < 0 || c.Alerting.MaxAlerts > 1000:
		return errors.New("connectors.alerting.maxAlerts must be between 0 and 1000")
	case c.Alerting.RequestsPerSecond < 0 || c.Alerting.RequestsPerSecond > 100:
		return errors.New("connectors.alerting.requestsPerSecond must be between 0 and 100")
	case c.Alerting.Burst < 0 || c.Alerting.Burst > 200:
		return errors.New("connectors.alerting.burst must be between 0 and 200")
	}
	for _, host := range c.AllowedHosts {
		if strings.ContainsAny(host, "/:@ ") || host == "" {
			return fmt.Errorf("connectors.allowedHosts entry %q is not a host name or IP address", host)
		}
	}
	return nil
}

// withDefaults fills every omitted bound with its documented default.
func (c Config) withDefaults() Config {
	c.Logs.NativeKubernetes.withDefaults()
	c.Logs.Loki.setDefaults()
	c.Logs.Grafana.setDefaults()
	c.Logs.CloudWatchLogs.setDefaults()
	c.Logs.DatadogLogs.setDefaults()
	c.Logs.OpenSearch.setDefaults()
	c.Logs.MCP.setDefaults()
	c.Metrics.Prometheus.setDefaults()
	c.Metrics.Grafana.setDefaults()
	c.Metrics.CloudWatch.setDefaults()
	c.Metrics.Datadog.setDefaults()
	c.Metrics.MCP.setDefaults()
	c.AlertState.Alertmanager.setDefaults()
	c.AlertState.Grafana.setDefaults()
	if c.Alerting.MaxAlerts <= 0 {
		c.Alerting.MaxAlerts = 50
	}
	if c.Alerting.RequestsPerSecond <= 0 {
		c.Alerting.RequestsPerSecond = 10
	}
	if c.Alerting.Burst <= 0 {
		c.Alerting.Burst = 20
	}
	return c
}

// withDefaults fills the native collector's bounds.
func (n *NativeConfig) withDefaults() {
	if n.TailLines <= 0 {
		n.TailLines = 500
	}
	if n.LimitBytes <= 0 {
		n.LimitBytes = 256 << 10
	}
	if n.SinceSeconds <= 0 {
		n.SinceSeconds = 3600
	}
	if n.MaxPods <= 0 {
		n.MaxPods = 5
	}
	if n.MaxContainersPerPod <= 0 {
		n.MaxContainersPerPod = 4
	}
	if n.Concurrency <= 0 {
		n.Concurrency = 8
	}
}

// validate enforces the native collector's bounds.
func (n NativeConfig) validate() error {
	switch {
	case n.TailLines < 0 || n.TailLines > 5000:
		return errors.New("connectors.logs.nativeKubernetes.tailLines must be between 0 and 5000")
	case n.LimitBytes < 0 || n.LimitBytes > 4<<20:
		return errors.New("connectors.logs.nativeKubernetes.limitBytes must be between 0 and 4194304")
	case n.SinceSeconds < 0 || n.SinceSeconds > 86400:
		return errors.New("connectors.logs.nativeKubernetes.sinceSeconds must be between 0 and 86400")
	case n.MaxPods < 0 || n.MaxPods > 50:
		return errors.New("connectors.logs.nativeKubernetes.maxPods must be between 0 and 50")
	case n.MaxContainersPerPod < 0 || n.MaxContainersPerPod > 20:
		return errors.New("connectors.logs.nativeKubernetes.maxContainersPerPod must be between 0 and 20")
	case n.Concurrency < 0 || n.Concurrency > 32:
		return errors.New("connectors.logs.nativeKubernetes.concurrency must be between 0 and 32")
	}
	return nil
}

// guard returns the connector's retry and overload policy.
func (g guard) normalized(timeout time.Duration, maxPages int) guard {
	if g.Timeout <= 0 {
		g.Timeout = Duration(timeout)
	}
	if g.MaxBytes <= 0 {
		g.MaxBytes = base.DefaultMaxBytes
	}
	if g.MaxPages <= 0 {
		g.MaxPages = maxPages
	}
	if g.RequestsPerSecond <= 0 {
		g.RequestsPerSecond = base.DefaultRequestsPerSecond
	}
	if g.Burst <= 0 {
		g.Burst = base.DefaultBurst
	}
	if g.FailureThreshold <= 0 {
		g.FailureThreshold = base.DefaultFailureThreshold
	}
	if g.Cooldown <= 0 {
		g.Cooldown = Duration(base.DefaultCooldown)
	}
	return g
}

// policy converts the guard to the base policy.
func (g guard) policy() base.Policy {
	return base.Policy{
		Timeout: g.Timeout.Std(), MaxBytes: g.MaxBytes, MaxPages: g.MaxPages,
		RequestsPerSecond: g.RequestsPerSecond, Burst: g.Burst,
		FailureThreshold: g.FailureThreshold, Cooldown: g.Cooldown.Std(),
	}
}

// validate enforces the shared bounds.
func (g guard) validate() error {
	switch {
	case g.Timeout < 0 || g.Timeout.Std() > base.MaxTimeout:
		return fmt.Errorf("timeout must be between 0 and %s", base.MaxTimeout)
	case g.MaxBytes < 0 || g.MaxBytes > base.MaxMaxBytes:
		return fmt.Errorf("maxBytes must be between 0 and %d", base.MaxMaxBytes)
	case g.MaxPages < 0 || g.MaxPages > base.MaxMaxPages:
		return fmt.Errorf("maxPages must be between 0 and %d", base.MaxMaxPages)
	case g.RequestsPerSecond < 0 || g.RequestsPerSecond > 100:
		return errors.New("requestsPerSecond must be between 0 and 100")
	case g.Burst < 0 || g.Burst > 200:
		return errors.New("burst must be between 0 and 200")
	case g.FailureThreshold < 0 || g.FailureThreshold > 100:
		return errors.New("failureThreshold must be between 0 and 100")
	case g.Cooldown < 0 || g.Cooldown.Std() > 10*time.Minute:
		return errors.New("cooldown must be between 0 and 10m")
	}
	return nil
}

// validateRedaction compiles every operator pattern.
func validateRedaction(config RedactionConfig) error {
	for i, pattern := range config.Patterns {
		if _, err := base.CompileRedaction(pattern); err != nil {
			return fmt.Errorf("connectors.redaction.patterns[%d]: %w", i, err)
		}
	}
	return nil
}

// validateEndpoint checks the fields that every enabled HTTP Connector needs.
func validateEndpoint(name string, enabled bool, rawURL string, auth Auth) error {
	if !enabled {
		return nil
	}
	if rawURL == "" {
		return fmt.Errorf("connectors.%s.url is required when the Connector is enabled", name)
	}
	if rawURL != "" {
		if err := base.ValidateURL(name, rawURL); err != nil {
			return err
		}
	}
	return validateAuth(name, auth)
}

// validateAuth rejects a reference that names an unresolved credential key.
// An inline value is rejected earlier by strict YAML decoding: a field named
// token or password is an unknown field, not a supported one.
func validateAuth(_ string, _ Auth) error {
	return nil
}

// validate validates the Loki Connector.
func (c LokiConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if err := validateEndpoint("logs.loki", c.Enabled, c.URL, c.Auth); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Limit <= 0 || c.Limit > 5000 {
		return errors.New("connectors.logs.loki.limit must be between 1 and 5000")
	}
	_, err := base.CompileTemplate("connectors.logs.loki.queryTemplate", c.QueryTemplate, true)
	return err
}

func (c LokiConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: c.MaxPages,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *LokiConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, base.DefaultMaxPages)
	c.Timeout, c.MaxBytes, c.MaxPages = g.Timeout, g.MaxBytes, g.MaxPages
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Limit <= 0 {
		c.Limit = 500
	}
	if c.QueryTemplate == "" {
		c.QueryTemplate = `{namespace="{{ .Namespace }}", pod=~"{{ .PodPrefix }}.*"}`
	}
}

// validate validates the Grafana Connector.
func (c GrafanaConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if err := validateEndpoint("logs.grafana", c.Enabled, c.URL, c.Auth); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.DatasourceUID == "" {
		return errors.New("connectors.logs.grafana.datasourceUID is required when the Connector is enabled")
	}
	if c.Mode != "" && c.Mode != "proxy" && c.Mode != "dsquery" {
		return errors.New("connectors.logs.grafana.mode must be proxy or dsquery")
	}
	if c.Limit <= 0 || c.Limit > 5000 {
		return errors.New("connectors.logs.grafana.limit must be between 1 and 5000")
	}
	_, err := base.CompileTemplate("connectors.logs.grafana.queryTemplate", c.QueryTemplate, false)
	return err
}

func (c GrafanaConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: c.MaxPages,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *GrafanaConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, base.DefaultMaxPages)
	c.Timeout, c.MaxBytes, c.MaxPages = g.Timeout, g.MaxBytes, g.MaxPages
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Mode == "" {
		c.Mode = "proxy"
	}
	if c.Limit <= 0 {
		c.Limit = 500
	}
	if c.Step <= 0 {
		c.Step = Duration(30 * time.Second)
	}
}

// validate validates the CloudWatch Logs Connector.
func (c CloudWatchLogsConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Region == "" {
		return errors.New("connectors.logs.cloudwatchLogs.region is required when the Connector is enabled")
	}
	if c.Auth.Mode != "" && c.Auth.Mode != authModeIRSA && c.Auth.Mode != authModeStatic {
		return errors.New("connectors.logs.cloudwatchLogs.auth.mode must be irsa or static")
	}
	if c.Auth.Mode == authModeStatic && (c.Auth.AccessKeyIDKey == "" || c.Auth.SecretAccessKeyKey == "") {
		return errors.New("connectors.logs.cloudwatchLogs.auth needs accessKeyIDKey and secretAccessKeyKey in static mode")
	}
	if c.Limit <= 0 || c.Limit > 10000 {
		return errors.New("connectors.logs.cloudwatchLogs.limit must be between 1 and 10000")
	}
	if c.Mode != "" && c.Mode != "filter" && c.Mode != "insights" {
		return errors.New("connectors.logs.cloudwatchLogs.mode must be filter or insights")
	}
	if _, err := base.CompileTemplate("connectors.logs.cloudwatchLogs.logGroupNameTemplate",
		c.LogGroupNameTemplate, true); err != nil {
		return err
	}
	return validateAuth("logs.cloudwatchLogs", c.Auth)
}

func (c CloudWatchLogsConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: c.MaxPages,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *CloudWatchLogsConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, 2)
	c.Timeout, c.MaxBytes, c.MaxPages = g.Timeout, g.MaxBytes, g.MaxPages
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Limit <= 0 {
		c.Limit = 1000
	}
	if c.Mode == "" {
		c.Mode = "filter"
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = authModeIRSA
	}
}

// validate validates the Datadog Logs Connector.
func (c DatadogLogsConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Limit <= 0 || c.Limit > 1000 {
		return errors.New("connectors.logs.datadogLogs.limit must be between 1 and 1000")
	}
	if c.Auth.APIKeyKey == "" || c.Auth.AppKeyKey == "" {
		return errors.New("connectors.logs.datadogLogs.auth needs apiKeyKey and appKeyKey")
	}
	if _, err := base.CompileTemplate("connectors.logs.datadogLogs.queryTemplate", c.QueryTemplate, true); err != nil {
		return err
	}
	return validateAuth("logs.datadogLogs", c.Auth)
}

func (c DatadogLogsConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: c.MaxPages,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *DatadogLogsConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, 2)
	c.Timeout, c.MaxBytes, c.MaxPages = g.Timeout, g.MaxBytes, g.MaxPages
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Limit <= 0 {
		c.Limit = 200
	}
	if c.Site == "" {
		c.Site = "datadoghq.com"
	}
	if c.QueryTemplate == "" {
		c.QueryTemplate = "service:{{ .Workload }}"
	}
}

// validate validates the OpenSearch Connector.
func (c OpenSearchConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if err := validateEndpoint("logs.opensearch", c.Enabled, c.URL, c.Auth); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Limit <= 0 || c.Limit > 5000 {
		return errors.New("connectors.logs.opensearch.limit must be between 1 and 5000")
	}
	if _, err := base.CompileTemplate("connectors.logs.opensearch.indexTemplate", c.IndexTemplate, true); err != nil {
		return err
	}
	if _, err := base.CompileTemplate("connectors.logs.opensearch.queryTemplate", c.QueryTemplate, false); err != nil {
		return err
	}
	return nil
}

func (c OpenSearchConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: c.MaxPages,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *OpenSearchConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, base.DefaultMaxPages)
	c.Timeout, c.MaxBytes, c.MaxPages = g.Timeout, g.MaxBytes, g.MaxPages
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Limit <= 0 {
		c.Limit = 500
	}
	if c.IndexTemplate == "" {
		c.IndexTemplate = "logs-{{ .Namespace }}-*"
	}
}

// validate validates the MCP Connector.
func (c MCPConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if err := validateEndpoint("logs.mcp", c.Enabled, c.URL, c.Auth); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Tool == "" {
		return errors.New("connectors.logs.mcp.tool is required when the Connector is enabled")
	}
	_, err := base.CompileTemplate("connectors.logs.mcp.argumentsTemplate", c.ArgumentsTemplate, true)
	return err
}

func (c MCPConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *MCPConfig) setDefaults() {
	g := c.guard().normalized(20*time.Second, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
}

// validate validates the Prometheus Connector.
func (c PrometheusConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if err := validateEndpoint("metrics.prometheus", c.Enabled, c.URL, c.Auth); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if len(c.Queries) == 0 {
		return errors.New("connectors.metrics.prometheus.queries needs at least one query")
	}
	for name, template := range c.Queries {
		if _, err := base.CompileTemplate("connectors.metrics.prometheus.queries."+name, template, true); err != nil {
			return err
		}
	}
	if c.MaxSamples < 0 || c.MaxSamples > 100000 {
		return errors.New("connectors.metrics.prometheus.maxSamples must be between 0 and 100000")
	}
	return nil
}

func (c PrometheusConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: 1,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *PrometheusConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Step <= 0 {
		c.Step = Duration(30 * time.Second)
	}
	if c.MaxSamples <= 0 {
		c.MaxSamples = 10000
	}
}

// validate validates the CloudWatch metrics Connector.
func (c CloudWatchMetricsConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if c.Region == "" {
		return errors.New("connectors.metrics.cloudwatch.region is required when the Connector is enabled")
	}
	if len(c.Queries) == 0 {
		return errors.New("connectors.metrics.cloudwatch.queries needs at least one query")
	}
	for name, template := range c.Queries {
		if _, err := base.CompileTemplate("connectors.metrics.cloudwatch.queries."+name, template, true); err != nil {
			return err
		}
	}
	if c.Auth.Mode == authModeStatic && (c.Auth.AccessKeyIDKey == "" || c.Auth.SecretAccessKeyKey == "") {
		return errors.New("connectors.metrics.cloudwatch.auth needs accessKeyIDKey and secretAccessKeyKey in static mode")
	}
	return validateAuth("metrics.cloudwatch", c.Auth)
}

func (c CloudWatchMetricsConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: 1,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *CloudWatchMetricsConfig) setDefaults() {
	g := c.guard().normalized(20*time.Second, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Auth.Mode == "" {
		c.Auth.Mode = authModeIRSA
	}
}

// validate validates the Datadog metrics Connector.
func (c DatadogMetricsConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	if !c.Enabled {
		return nil
	}
	if len(c.Queries) == 0 {
		return errors.New("connectors.metrics.datadog.queries needs at least one query")
	}
	for name, template := range c.Queries {
		if _, err := base.CompileTemplate("connectors.metrics.datadog.queries."+name, template, true); err != nil {
			return err
		}
	}
	if c.Auth.APIKeyKey == "" || c.Auth.AppKeyKey == "" {
		return errors.New("connectors.metrics.datadog.auth needs apiKeyKey and appKeyKey")
	}
	return validateAuth("metrics.datadog", c.Auth)
}

func (c DatadogMetricsConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: 1,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *DatadogMetricsConfig) setDefaults() {
	g := c.guard().normalized(base.DefaultTimeout, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
	if c.Site == "" {
		c.Site = "datadoghq.com"
	}
}

// validate validates the Alertmanager Connector.
func (c AlertmanagerConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	return validateEndpoint("alertState.alertmanager", c.Enabled, c.URL, c.Auth)
}

func (c AlertmanagerConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: 1,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *AlertmanagerConfig) setDefaults() {
	g := c.guard().normalized(10*time.Second, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
}

// validate validates the Grafana alert-state Connector.
func (c GrafanaAlertStateConfig) validate() error {
	if err := c.guard().validate(); err != nil {
		return err
	}
	return validateEndpoint("alertState.grafana", c.Enabled, c.URL, c.Auth)
}

func (c GrafanaAlertStateConfig) guard() guard {
	return guard{Timeout: c.Timeout, MaxBytes: c.MaxBytes, MaxPages: 1,
		RequestsPerSecond: c.RequestsPerSecond, Burst: c.Burst}
}

func (c *GrafanaAlertStateConfig) setDefaults() {
	g := c.guard().normalized(10*time.Second, 1)
	c.Timeout, c.MaxBytes = g.Timeout, g.MaxBytes
	c.RequestsPerSecond, c.Burst = g.RequestsPerSecond, g.Burst
}

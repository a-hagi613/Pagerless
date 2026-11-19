package connectors

import (
	"strings"
	"testing"
)

// minimal is the smallest valid Connector configuration: mandatory native logs
// and no optional backend.
const minimal = `
logs:
  nativeKubernetes:
    enabled: true
`

func TestParseConfigAppliesTheNativeDefaults(t *testing.T) {
	t.Parallel()
	config, err := ParseConfig([]byte(minimal))
	if err != nil {
		t.Fatalf("parse the minimal configuration: %v", err)
	}
	native := config.Logs.NativeKubernetes
	if !native.Enabled {
		t.Error("native logs = disabled, want the mandatory collector")
	}
	if native.TailLines != 500 || native.LimitBytes != 256<<10 || native.SinceSeconds != 3600 {
		t.Errorf("native bounds = %+v, want the documented defaults", native)
	}
	if config.Alerting.MaxAlerts != 50 || config.Alerting.RequestsPerSecond != 10 || config.Alerting.Burst != 20 {
		t.Errorf("alerting defaults = %+v, want the documented webhook bounds", config.Alerting)
	}
}

func TestParseConfigRefusesAConfigurationThatDisablesNativeLogs(t *testing.T) {
	t.Parallel()
	_, err := ParseConfig([]byte(`
logs:
  nativeKubernetes:
    enabled: false
`))
	if err == nil || !strings.Contains(err.Error(), "nativeKubernetes.enabled must be true") {
		t.Fatalf("error = %v, want a refusal that explains the mandatory collector", err)
	}
}

func TestParseConfigRefusesASecretValueAndAnUnknownField(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a token value": `
logs:
  nativeKubernetes:
    enabled: true
  loki:
    enabled: true
    url: https://loki.monitoring.svc:3100
    token: super-secret
`,
		"a typo": `
logs:
  nativeKubernetes:
    enabled: true
  lokii:
    enabled: true
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseConfig([]byte(body)); err == nil {
				t.Fatal("ParseConfig accepted an unknown field, want a boot failure")
			}
		})
	}
}

func TestParseConfigValidatesQueryTemplatesAtStartup(t *testing.T) {
	t.Parallel()
	_, err := ParseConfig([]byte(`
logs:
  nativeKubernetes:
    enabled: true
  loki:
    enabled: true
    url: https://loki.monitoring.svc:3100
    limit: 500
    queryTemplate: '{namespace="{{ .Namespace }}", pod=~"{{ .Unknown }}"}'
`))
	if err == nil || !strings.Contains(err.Error(), "connectors.logs.loki.queryTemplate") {
		t.Fatalf("error = %v, want a template failure that names the field", err)
	}
}

func TestParseConfigValidatesAWebhookExtractionRule(t *testing.T) {
	t.Parallel()
	_, err := ParseConfig([]byte(`
logs:
  nativeKubernetes:
    enabled: true
subject:
  rules:
    - label: alertname
      kind: Deployment
      pattern: '^(?P<name>[a-z-]+)$'
`))
	if err == nil || !strings.Contains(err.Error(), "connectors.subject.rules[0]") {
		t.Fatalf("error = %v, want a refusal that names the rule", err)
	}
}

func TestParseConfigBoundsTheWebhookLimits(t *testing.T) {
	t.Parallel()
	_, err := ParseConfig([]byte(`
logs:
  nativeKubernetes:
    enabled: true
alerting:
  maxAlerts: 5000
`))
	if err == nil || !strings.Contains(err.Error(), "maxAlerts") {
		t.Fatalf("error = %v, want the webhook alert bound", err)
	}
}

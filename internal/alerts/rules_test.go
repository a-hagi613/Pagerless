package alerts_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// rulesDir holds the PrometheusRule manifests that Argo CD deploys into the
// monitoring namespace.
const rulesDir = "../../deploy/observability/alerts"

// rule is one rule of a PrometheusRule: an alerting rule when Record is empty,
// and a recording rule otherwise.
type rule struct {
	Record      string            `json:"record"`
	Alert       string            `json:"alert"`
	Expr        string            `json:"expr"`
	For         string            `json:"for"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

// prometheusRule is the part of a PrometheusRule that these tests read.
type prometheusRule struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []rule `json:"rules"`
		} `json:"groups"`
	} `json:"spec"`
}

// rules is every rule in rulesDir: the alerting rules keyed by alert name, and
// the recording rules keyed by the series they record.
type rules struct {
	alerts     map[string]rule
	recordings map[string]rule
}

// loadRules reads every PrometheusRule in rulesDir and fails when a file is not
// one, repeats a name, or defines a rule that is neither an alert nor a
// recording.
func loadRules(t *testing.T) rules {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(rulesDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s defines no PrometheusRule", rulesDir)
	}
	loaded := rules{alerts: make(map[string]rule), recordings: make(map[string]rule)}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file prometheusRule
		if err := yaml.UnmarshalStrict(raw, &file); err != nil {
			t.Fatalf("%s is not a valid PrometheusRule: %v", path, err)
		}
		if file.APIVersion != "monitoring.coreos.com/v1" || file.Kind != "PrometheusRule" {
			t.Errorf("%s is %s %s, want a monitoring.coreos.com/v1 PrometheusRule",
				path, file.APIVersion, file.Kind)
		}
		if file.Metadata.Name == "" {
			t.Errorf("%s names no rule", path)
		}
		// The Alertmanager route and the receiver are configured for this namespace.
		if file.Metadata.Namespace != "monitoring" {
			t.Errorf("%s is in namespace %q, want monitoring", path, file.Metadata.Namespace)
		}
		if len(file.Spec.Groups) == 0 {
			t.Errorf("%s defines no rule group", path)
		}
		for _, group := range file.Spec.Groups {
			if group.Name == "" {
				t.Errorf("%s defines a group with no name", path)
			}
			if len(group.Rules) == 0 {
				t.Errorf("%s defines group %q with no rule", path, group.Name)
			}
			for _, entry := range group.Rules {
				if strings.TrimSpace(entry.Expr) == "" {
					t.Errorf("%s defines a rule without an expression", path)
					continue
				}
				switch {
				case entry.Alert != "" && entry.Record == "":
					if _, seen := loaded.alerts[entry.Alert]; seen {
						t.Errorf("%s defines the alert %s a second time", path, entry.Alert)
					}
					loaded.alerts[entry.Alert] = entry
				case entry.Record != "" && entry.Alert == "":
					if _, seen := loaded.recordings[entry.Record]; seen {
						t.Errorf("%s defines the recording %s a second time", path, entry.Record)
					}
					loaded.recordings[entry.Record] = entry
				default:
					t.Errorf("%s defines a rule that is neither one alert nor one recording: %+v", path, entry)
				}
			}
		}
	}
	return loaded
}

// The receiver of pagerless-server reads pagerless_triage, namespace and exactly
// one of deployment and statefulset from every alert it turns into a Triage. The
// label names each rule emits are asserted by that rule's own test below; this
// one covers what every alert must share.
func TestEveryAlertOptsIn(t *testing.T) {
	t.Parallel()
	loaded := loadRules(t)
	for _, want := range []string{"ContainerOOMKilled", "ServiceErrorRateHigh", "ContainerMemoryGrowth"} {
		if _, ok := loaded.alerts[want]; !ok {
			t.Errorf("%s defines no %s rule", rulesDir, want)
		}
	}
	for name, alert := range loaded.alerts {
		t.Run(name, func(t *testing.T) {
			if got := alert.Labels["pagerless_triage"]; got != "true" {
				t.Errorf("label pagerless_triage = %q, want true", got)
			}
			if alert.Labels["severity"] == "" {
				t.Error("the rule sets no severity label")
			}
			if alert.Annotations["summary"] == "" || alert.Annotations["description"] == "" {
				t.Error("the rule has no summary or no description")
			}
		})
	}
}

// The error-rate alert reads the rate of failing server spans, which the
// span-metrics connector of the demo's collector produces. The values below are
// the ones read from the live metric: the connector reports an error span's
// status.code as STATUS_CODE_ERROR, and Prometheus normalizes the dotted labels.
// The rule skips the four Workloads that report errors in the demo's known-good
// state, and it requires both a share of failing spans and an absolute rate.
func TestServiceErrorRateHighReadsTheSpanMetricsErrorStatus(t *testing.T) {
	t.Parallel()
	loaded := loadRules(t)
	alert, ok := loaded.alerts["ServiceErrorRateHigh"]
	if !ok {
		t.Fatal("ServiceErrorRateHigh is not defined")
	}
	for _, want := range []string{
		"pagerless:service_server_error_ratio:5m > 0.01",
		"pagerless:service_server_errors:rate5m > 0.005",
	} {
		if !strings.Contains(alert.Expr, want) {
			t.Errorf("the expression lacks %q:\n%s", want, alert.Expr)
		}
	}
	if alert.For != "3m" {
		t.Errorf("for = %q, want 3m", alert.For)
	}

	ratio, ok := loaded.recordings["pagerless:service_server_error_ratio:5m"]
	if !ok {
		t.Fatal("the ratio recording rule is not defined")
	}
	for _, want := range []string{
		"traces_span_metrics_calls_total",
		`status_code="STATUS_CODE_ERROR"`,
		`span_kind="SPAN_KIND_SERVER"`,
		`"deployment", "$1", "service_name", "(.+)"`,
		`"namespace", "$1", "k8s_namespace_name", "(.+)"`,
		`"namespace", "sandbox", "namespace", "^$"`,
		`service_name!~"frontend|frontend-proxy|checkout|shipping"`,
		"[5m]",
	} {
		if !strings.Contains(ratio.Expr, want) {
			t.Errorf("the ratio expression lacks %q:\n%s", want, ratio.Expr)
		}
	}
	errors, ok := loaded.recordings["pagerless:service_server_errors:rate5m"]
	if !ok {
		t.Fatal("the error-rate recording rule is not defined")
	}
	if !strings.Contains(errors.Expr, `status_code="STATUS_CODE_ERROR"`) ||
		!strings.Contains(errors.Expr, `span_kind="SPAN_KIND_SERVER"`) {
		t.Errorf("the error-rate expression does not count failing server spans:\n%s", errors.Expr)
	}
}

// The memory rule compares a container with its own state ten minutes earlier and
// with its own limit, and ignores a container that has not been running for a
// quarter of an hour, so that startup ramps never alert.
func TestContainerMemoryGrowthComparesAContainerWithItsOwnHistoryAndLimit(t *testing.T) {
	t.Parallel()
	alert, ok := loadRules(t).alerts["ContainerMemoryGrowth"]
	if !ok {
		t.Fatal("ContainerMemoryGrowth is not defined")
	}
	for _, want := range []string{
		"container_memory_working_set_bytes",
		"offset 10m",
		`kube_pod_container_resource_limits{resource="memory"}`,
		"kube_pod_created",
		"kube_pod_owner",
		"kube_replicaset_owner",
		"group_left (deployment)",
		"> 0.1",
		"> 900",
	} {
		if !strings.Contains(alert.Expr, want) {
			t.Errorf("the expression lacks %q:\n%s", want, alert.Expr)
		}
	}
	if alert.For != "1m" {
		t.Errorf("for = %q, want 1m", alert.For)
	}
	if strings.Contains(alert.Expr, "kube_pod_container_status_last_terminated_reason") {
		t.Error("the memory rule duplicates the OOMKilled rule instead of measuring growth")
	}
}

// The OOMKilled rule is Phase 1 behavior that this Scenario work must not change:
// it still opts in, still selects the OOMKilled termination, and still fires
// without a duration, because the termination is the event itself.
func TestTheOOMKilledRuleIsUnchanged(t *testing.T) {
	t.Parallel()
	alert, ok := loadRules(t).alerts["ContainerOOMKilled"]
	if !ok {
		t.Fatal("ContainerOOMKilled is not defined")
	}
	if alert.Labels["pagerless_triage"] != "true" {
		t.Errorf("label pagerless_triage = %q, want true", alert.Labels["pagerless_triage"])
	}
	if !strings.Contains(alert.Expr, `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`) {
		t.Error("the expression no longer selects OOMKilled terminations")
	}
	if !strings.Contains(alert.Expr, "max by (namespace, pod, deployment)") ||
		!strings.Contains(alert.Expr, "statefulset") {
		t.Error("the expression no longer joins every pod to a namespace and a Deployment or StatefulSet")
	}
	if !slices.Contains([]string{"", "0m"}, alert.For) {
		t.Errorf("for = %q, want the rule to fire on the termination itself", alert.For)
	}
}

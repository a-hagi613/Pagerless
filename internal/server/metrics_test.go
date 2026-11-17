package server_test

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/metrics"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

// triageLabel is the label that names one Triage on the per-Triage usage gauges.
const triageLabel = "triage"

func TestMetricsAfterAFullPhaseSequence(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	name := recovered(t, env)

	waitForMetric(t, env, metrics.MetricTriages, map[string]string{"phase": string(v1alpha1.PhaseRecovered)}, 1)
	waitForMetric(t, env, metrics.MetricProposals, map[string]string{
		"action": string(v1alpha1.ActionPatchResources),
	}, 1)
	waitForMetric(t, env, metrics.MetricDecisions, map[string]string{
		"decision": string(v1alpha1.DecisionApproved),
	}, 1)
	waitForMetric(t, env, metrics.MetricVerifications, map[string]string{
		"outcome": string(v1alpha1.PhaseRecovered),
	}, 1)
	waitForMetric(t, env, metrics.MetricInputTokens, map[string]string{triageLabel: name},
		float64(reportUsage.InputTokens))
	waitForMetric(t, env, metrics.MetricOutputTokens, map[string]string{triageLabel: name},
		float64(reportUsage.OutputTokens))
	waitForMetric(t, env, metrics.MetricCostUSD, map[string]string{triageLabel: name}, reportUsage.CostUSD)
	waitForMetric(t, env, metrics.MetricTimeToRecovered, nil, 1)

	histogram := metricFamily(t, scrape(t, env), metrics.MetricTimeToRecovered).Metric[0].GetHistogram()
	if histogram.GetSampleCount() != 1 || histogram.GetSampleSum() != 300 {
		t.Errorf("time to Recovered = %d samples summing %g, want 1 sample of 300s",
			histogram.GetSampleCount(), histogram.GetSampleSum())
	}
}

func TestMetricsRecordANotRecoveredVerification(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t, withPatchResources())
	tr, commit := merged(t, env, alertTrigger)
	// The triggering alert stays firing, so Verification ends NotRecovered.
	env.Alertmanager.Fire(alertFingerprint)
	verifying(t, env, tr, commit)
	env.Clock.Step(5 * time.Minute)
	env.WaitForPhase(t, tr.Name, v1alpha1.PhaseNotRecovered)

	waitForMetric(t, env, metrics.MetricVerifications, map[string]string{
		"outcome": string(v1alpha1.PhaseNotRecovered),
	}, 1)
}

// scrape reads the metrics API in the Prometheus text format.
func scrape(t *testing.T, env *testenv.Env) map[string]*dto.MetricFamily {
	t.Helper()
	response, err := http.Get(env.MetricsURL + "/metrics")
	if err != nil {
		t.Fatalf("scrape the metrics API: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the metrics API answered %d, want 200", response.StatusCode)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(response.Body)
	if err != nil {
		t.Fatalf("parse the metrics: %v", err)
	}
	return families
}

// waitForMetric polls until the metric has the value.
func waitForMetric(t *testing.T, env *testenv.Env, name string, labels map[string]string, want float64) {
	t.Helper()
	eventually(t, fmt.Sprintf("metric %s%v is %g", name, labels, want), func() bool {
		return metricEquals(scrape(t, env), name, labels, want)
	})
}

// metricFamily returns a metric family or fails the test.
func metricFamily(t *testing.T, families map[string]*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	family, ok := families[name]
	if !ok {
		names := make([]string, 0, len(families))
		for name := range families {
			names = append(names, name)
		}
		slices.Sort(names)
		t.Fatalf("metric %s is not exported; exported metrics: %v", name, names)
	}
	return family
}

// metricEquals reports whether a metric with the labels has the value. A histogram is compared by its sample count.
func metricEquals(families map[string]*dto.MetricFamily, name string, labels map[string]string, want float64) bool {
	family, ok := families[name]
	if !ok {
		return false
	}
	for _, metric := range family.Metric {
		if hasLabels(metric.Label, labels) {
			return math.Abs(metricNumber(metric)-want) < 1e-9
		}
	}
	return false
}

// hasLabels reports whether the metric carries every label and value.
func hasLabels(pairs []*dto.LabelPair, want map[string]string) bool {
	for key, value := range want {
		found := false
		for _, pair := range pairs {
			if pair.GetName() == key && pair.GetValue() == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// metricNumber returns the numeric value of a gauge or counter, or the sample count of a histogram.
func metricNumber(metric *dto.Metric) float64 {
	switch {
	case metric.Gauge != nil:
		return metric.GetGauge().GetValue()
	case metric.Counter != nil:
		return metric.GetCounter().GetValue()
	case metric.Histogram != nil:
		return float64(metric.GetHistogram().GetSampleCount())
	case metric.Untyped != nil:
		return metric.GetUntyped().GetValue()
	default:
		return math.NaN()
	}
}

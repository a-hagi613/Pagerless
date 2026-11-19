// Package metrics exports Pagerless's own behaviour to Prometheus: Pages by Phase, Proposals by Action,
// decisions, Verification outcomes, time to Recovered, and the tokens and cost of each Page.
//
// The counters are incremented as pagerless-server writes status. The gauges are collected from the Page cache at
// scrape time, so they survive a restart and drop a Page when retention deletes it.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

// Metric names. The dashboard and the tests refer to these names.
const (
	// MetricPages is the number of Pages in each Phase.
	MetricPages = "pagerless_pages"
	// MetricProposals counts validated Proposals by Action.
	MetricProposals = "pagerless_proposals_total"
	// MetricDecisions counts human decisions by outcome.
	MetricDecisions = "pagerless_decisions_total"
	// MetricVerifications counts Verification outcomes.
	MetricVerifications = "pagerless_verifications_total"
	// MetricTimeToRecovered is the time from the Page's start to Recovered.
	MetricTimeToRecovered = "pagerless_time_to_recovered_seconds"
	// MetricInputTokens is the input tokens one Page's investigation read.
	MetricInputTokens = "pagerless_page_input_tokens"
	// MetricOutputTokens is the output tokens one Page's investigation wrote.
	MetricOutputTokens = "pagerless_page_output_tokens"
	// MetricCostUSD is the estimated model cost of one Page in United States dollars.
	MetricCostUSD = "pagerless_page_cost_usd"
)

// labelPage is the label that names one Page on the per-Page usage gauges.
const labelPage = "page"

// phases are the labels of the Pages gauge. An empty phase is reported as Pending, because a Page that the
// server has not yet initialized is active and is about to be investigated.
var phases = []v1alpha1.Phase{
	v1alpha1.PhasePending, v1alpha1.PhaseInvestigating, v1alpha1.PhaseProposed, v1alpha1.PhaseApproved,
	v1alpha1.PhasePullRequestOpen, v1alpha1.PhaseVerifying, v1alpha1.PhaseRecovered, v1alpha1.PhaseNotRecovered,
	v1alpha1.PhaseDiagnosed, v1alpha1.PhaseRejected, v1alpha1.PhaseExpired, v1alpha1.PhaseInvalid,
	v1alpha1.PhaseFailed,
}

// Metrics owns one registry, so several pagerless-server managers in one process (as the envtest harness runs them)
// do not share or duplicate collectors.
type Metrics struct {
	registry        *prometheus.Registry
	proposals       *prometheus.CounterVec
	decisions       *prometheus.CounterVec
	verifications   *prometheus.CounterVec
	timeToRecovered prometheus.Histogram
}

// New builds the collectors and registers them. The reader is the Page cache that the gauges are collected from.
func New(reader client.Reader) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		proposals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricProposals,
			Help: "Validated Proposals by Action.",
		}, []string{"action"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricDecisions,
			Help: "Human decisions on Proposals by outcome.",
		}, []string{"decision"}),
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricVerifications,
			Help: "Verification outcomes.",
		}, []string{"outcome"}),
		timeToRecovered: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: MetricTimeToRecovered,
			Help: "Seconds from the Page start to the Recovered phase.",
			// A Page is bounded by the five-minute agent budget and the five-minute Verification window, but a
			// pull request may wait for a human to merge it, so the buckets reach one hour.
			Buckets: []float64{30, 60, 120, 300, 600, 900, 1800, 3600},
		}),
	}
	m.registry.MustRegister(
		m.proposals, m.decisions, m.verifications, m.timeToRecovered, newPageCollector(reader),
	)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObservePhaseChange records the counters that a status write implies. It is called after the status is written,
// with the status before the write, the status after it, and the time of the write.
func (m *Metrics) ObservePhaseChange(before, after *v1alpha1.Page, now time.Time) {
	if proposal := after.Status.Proposal; proposal != nil &&
		(before.Status.Proposal == nil || before.Status.Proposal.Hash != proposal.Hash) {
		m.proposals.WithLabelValues(string(proposal.Action)).Inc()
	}
	if before.Status.Decision == nil && after.Status.Decision != nil {
		m.decisions.WithLabelValues(string(after.Status.Decision.Outcome)).Inc()
	}
	if before.Status.Phase == after.Status.Phase {
		return
	}
	switch after.Status.Phase {
	case v1alpha1.PhaseRecovered:
		m.verifications.WithLabelValues(string(v1alpha1.PhaseRecovered)).Inc()
		if started := after.Status.StartedAt; started != nil {
			m.timeToRecovered.Observe(now.Sub(started.Time).Seconds())
		} else if !after.CreationTimestamp.IsZero() {
			m.timeToRecovered.Observe(now.Sub(after.CreationTimestamp.Time).Seconds())
		}
	case v1alpha1.PhaseNotRecovered:
		m.verifications.WithLabelValues(string(v1alpha1.PhaseNotRecovered)).Inc()
	}
}

// pageCollector reports the current Pages by Phase and the usage each Page recorded.
type pageCollector struct {
	reader       client.Reader
	pages        *prometheus.Desc
	inputTokens  *prometheus.Desc
	outputTokens *prometheus.Desc
	costUSD      *prometheus.Desc
}

// newPageCollector builds the descriptors once, before the registry can call Collect concurrently.
func newPageCollector(reader client.Reader) *pageCollector {
	return &pageCollector{
		reader: reader,
		pages:  prometheus.NewDesc(MetricPages, "Pages by Phase.", []string{"phase"}, nil),
		inputTokens: prometheus.NewDesc(
			MetricInputTokens, "Input tokens one Page's investigation read.", []string{labelPage}, nil,
		),
		outputTokens: prometheus.NewDesc(
			MetricOutputTokens, "Output tokens one Page's investigation wrote.", []string{labelPage}, nil,
		),
		costUSD: prometheus.NewDesc(
			MetricCostUSD, "Estimated model cost of one Page in United States dollars.", []string{labelPage}, nil,
		),
	}
}

// Describe sends the descriptors of the collector.
func (c *pageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pages
	ch <- c.inputTokens
	ch <- c.outputTokens
	ch <- c.costUSD
}

// Collect lists the Pages and emits one gauge per Phase and one gauge per Page that recorded usage.
func (c *pageCollector) Collect(ch chan<- prometheus.Metric) {
	var list v1alpha1.PageList
	// Prometheus calls Collect without a context. The cached read is bounded by the informer, not by a request.
	if err := c.reader.List(context.Background(), &list); err != nil {
		return
	}
	counts := make(map[v1alpha1.Phase]int, len(phases))
	for i := range list.Items {
		tr := &list.Items[i]
		phase := tr.Status.Phase
		if phase == "" {
			phase = v1alpha1.PhasePending
		}
		counts[phase]++
		if usage := tr.Status.Usage; usage != nil {
			ch <- prometheus.MustNewConstMetric(c.inputTokens, prometheus.GaugeValue, float64(usage.InputTokens), tr.Name)
			ch <- prometheus.MustNewConstMetric(c.outputTokens, prometheus.GaugeValue, float64(usage.OutputTokens), tr.Name)
			ch <- prometheus.MustNewConstMetric(c.costUSD, prometheus.GaugeValue, usage.CostUSD, tr.Name)
		}
	}
	for _, phase := range phases {
		ch <- prometheus.MustNewConstMetric(c.pages, prometheus.GaugeValue, float64(counts[phase]), string(phase))
	}
}

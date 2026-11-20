package base

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metric names for the Connector health surface.
const (
	// MetricRequests counts Connector calls by outcome.
	MetricRequests = "pagerless_connector_requests_total"
	// MetricErrors counts Connector failures by reason.
	MetricErrors = "pagerless_connector_errors_total"
	// MetricLatency is the latency of a Connector call.
	MetricLatency = "pagerless_connector_latency_seconds"
	// MetricBreakerState is the Circuit Breaker state: 0 closed, 1 half-open, 2 open.
	MetricBreakerState = "pagerless_connector_breaker_state"
)

// metricLabelConnector is the metric label that names a Connector.
const metricLabelConnector = "connector"

// Metrics records Connector health. A degraded Connector increments an error
// counter; it never blocks the Page that needed the evidence.
type Metrics struct {
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	breaker  *prometheus.GaugeVec
}

// NewMetrics builds and registers the Connector collectors. A nil registerer
// gets a private registry, which keeps tests isolated. A duplicate registration
// is returned as an error rather than silently adopting or panicking.
func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	if registerer == nil {
		registerer = prometheus.NewRegistry()
	}
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricRequests, Help: "Connector calls by outcome.",
		}, []string{metricLabelConnector, "outcome"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricErrors, Help: "Connector failures by reason.",
		}, []string{metricLabelConnector, "reason"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: MetricLatency, Help: "Connector call latency in seconds.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20},
		}, []string{metricLabelConnector}),
		breaker: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricBreakerState, Help: "Connector Circuit Breaker state: 0 closed, 1 half-open, 2 open.",
		}, []string{metricLabelConnector}),
	}
	for _, collector := range []struct {
		name      string
		collector prometheus.Collector
	}{
		{MetricRequests, m.requests},
		{MetricErrors, m.errors},
		{MetricLatency, m.latency},
		{MetricBreakerState, m.breaker},
	} {
		if err := register(registerer, collector.name, collector.collector); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// register registers a collector and reports a collision with both the
// existing and the new descriptor, so a duplicate is a startup error rather
// than a stack trace.
func register(registerer prometheus.Registerer, name string, collector prometheus.Collector) error {
	if err := registerer.Register(collector); err != nil {
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			return fmt.Errorf("metric %s collides with an existing registration: existing %s, new %s",
				name, describe(already.ExistingCollector), describe(collector))
		}
		return fmt.Errorf("register metric %s: %w", name, err)
	}
	return nil
}

// describe names a collector's descriptors for a collision message.
func describe(collector prometheus.Collector) string {
	descriptions := make(chan *prometheus.Desc, 8)
	go func() {
		collector.Describe(descriptions)
		close(descriptions)
	}()
	var names []string
	for description := range descriptions {
		names = append(names, description.String())
	}
	return strings.Join(names, "; ")
}

// ObserveRequest records one call.
func (m *Metrics) ObserveRequest(connector, outcome string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(connector, outcome).Inc()
	m.latency.WithLabelValues(connector).Observe(elapsed.Seconds())
}

// ObserveError records one failure.
func (m *Metrics) ObserveError(connector, reason string) {
	if m == nil {
		return
	}
	m.errors.WithLabelValues(connector, reason).Inc()
}

// ObserveBreaker records the Circuit Breaker state.
func (m *Metrics) ObserveBreaker(connector string, state State) {
	if m == nil {
		return
	}
	value := 0.0
	switch state {
	case StateHalfOpen:
		value = 1
	case StateOpen:
		value = 2
	}
	m.breaker.WithLabelValues(connector).Set(value)
}

// State is a Circuit Breaker state.
type State int

const (
	// StateClosed passes every call.
	StateClosed State = iota
	// StateHalfOpen passes one probe after the cool-off.
	StateHalfOpen
	// StateOpen fails every call until the cool-off elapses.
	StateOpen
)

// Breaker is a per-Connector circuit breaker. It opens after a run of
// consecutive failures and half-opens one probe after the cool-off, so a
// broken backend costs one request every cool-off rather than a timeout on
// every Page.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	onState   func(State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// NewBreaker builds a breaker. onState may be nil.
func NewBreaker(threshold int, cooldown time.Duration, now func() time.Time, onState func(State)) *Breaker {
	if threshold <= 0 {
		threshold = DefaultFailureThreshold
	}
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	if now == nil {
		now = time.Now
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: now, onState: onState}
}

// Allow reports whether a call may proceed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.transition(StateHalfOpen)
		b.probing = true
		return true
	case StateHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// Record reports the outcome of a call that Allow admitted.
func (b *Breaker) Record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if err == nil {
		b.failures = 0
		if b.state != StateClosed {
			b.transition(StateClosed)
		}
		return
	}
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.threshold {
		b.openedAt = b.now()
		b.transition(StateOpen)
	}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// transition records a state change. The caller holds the lock.
func (b *Breaker) transition(state State) {
	if b.state == state {
		return
	}
	b.state = state
	if b.onState != nil {
		b.onState(state)
	}
}

// Limiter is a per-Connector token bucket. A call that finds no token fails
// fast and degrades the bundle instead of waiting and holding up the Page.
type Limiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewLimiter builds a limiter that admits rate requests per second with the
// given burst.
func NewLimiter(rate float64, burst int, now func() time.Time) *Limiter {
	if rate <= 0 {
		rate = DefaultRequestsPerSecond
	}
	if burst <= 0 {
		burst = DefaultBurst
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, tokens: float64(burst), last: now()}
}

// Take reports whether one request may proceed now.
func (l *Limiter) Take() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	elapsed := now.Sub(l.last).Seconds()
	if elapsed > 0 {
		l.tokens = min(l.burst, l.tokens+elapsed*l.rate)
		l.last = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// httpStatusIsServerError reports whether the status is a retryable server error.
func httpStatusIsServerError(status int) bool {
	return status >= http.StatusInternalServerError
}

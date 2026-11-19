package webhooks

import (
	"context"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// rateLimiter is a token bucket that bounds one source's deliveries.
type rateLimiter struct {
	mutex  sync.Mutex
	tokens float64
	last   time.Time
}

// allow takes one token and reports whether the delivery may proceed.
func (l *rateLimiter) allow(now time.Time, requestsPerSecond float64, burst int) bool {
	if requestsPerSecond <= 0 {
		requestsPerSecond = DefaultRequestsPerSecond
	}
	if burst <= 0 {
		burst = DefaultBurst
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.last.IsZero() {
		l.tokens, l.last = float64(burst), now
	}
	l.tokens = min(float64(burst), l.tokens+now.Sub(l.last).Seconds()*requestsPerSecond)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// allow applies the per-source rate limit. It runs before decoding, so a
// sender cannot spend the decoder's work while it is over its rate.
func (r *Receiver) allow(source Source) bool {
	r.mutex.Lock()
	if r.limiters == nil {
		r.limiters = make(map[Source]*rateLimiter, 5)
	}
	limiter, ok := r.limiters[source]
	if !ok {
		limiter = &rateLimiter{}
		r.limiters[source] = limiter
	}
	r.mutex.Unlock()
	return limiter.allow(r.now(), r.RequestsPerSecond, r.Burst)
}

// prune deletes deliveries older than the retention window at most once a
// minute, so a busy sender does not run a DELETE for every delivery.
func (r *Receiver) prune(ctx context.Context) {
	if r.Deliveries == nil {
		return
	}
	now := r.now()
	r.mutex.Lock()
	due := r.lastPrune.IsZero() || now.Sub(r.lastPrune) >= time.Minute
	if due {
		r.lastPrune = now
	}
	r.mutex.Unlock()
	if !due {
		return
	}
	if _, err := r.Deliveries.PruneDeliveries(ctx, now.Add(-DeliveryRetention)); err != nil {
		logf.FromContext(ctx).Error(err, "prune webhook deliveries")
	}
}

// now returns the receiver's clock time.
func (r *Receiver) now() time.Time {
	if r.Clock == nil {
		return time.Now().UTC()
	}
	return r.Clock.Now().UTC()
}

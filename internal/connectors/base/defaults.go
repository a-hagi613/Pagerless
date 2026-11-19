package base

import "time"

// Defaults bound every Connector even when the configuration omits a field.
const (
	// DefaultTimeout bounds one backend call.
	DefaultTimeout = 15 * time.Second
	// MaxTimeout is the longest timeout an operator may configure.
	MaxTimeout = 60 * time.Second
	// DefaultMaxBytes caps one backend response body.
	DefaultMaxBytes = 256 << 10
	// MaxMaxBytes is the largest response body cap an operator may configure.
	MaxMaxBytes = 1 << 20
	// DefaultMaxPages is the number of pages an ordinary Connector reads.
	DefaultMaxPages = 1
	// MaxMaxPages is the largest page count a multi-page Connector may read.
	MaxMaxPages = 3
	// DefaultRequestsPerSecond is the per-Connector request rate.
	DefaultRequestsPerSecond = 2
	// DefaultBurst is the per-Connector burst allowance.
	DefaultBurst = 4
	// DefaultFailureThreshold opens a Circuit Breaker after this many consecutive failures.
	DefaultFailureThreshold = 5
	// DefaultCooldown is how long an open Circuit Breaker waits before one probe.
	DefaultCooldown = 30 * time.Second
)

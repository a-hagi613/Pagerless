package base

import "time"

// Policy is the timeout, byte cap, retry and overload policy every Connector
// shares.
type Policy struct {
	// Timeout bounds one backend call.
	Timeout time.Duration
	// MaxBytes caps one response body.
	MaxBytes int64
	// MaxPages is the number of pages a multi-page Connector reads.
	MaxPages int
	// RequestsPerSecond is the per-Connector request rate.
	RequestsPerSecond float64
	// Burst is the per-Connector burst allowance.
	Burst int
	// FailureThreshold opens the Circuit Breaker after this many failures.
	FailureThreshold int
	// Cooldown is how long an open Circuit Breaker waits before one probe.
	Cooldown time.Duration
}

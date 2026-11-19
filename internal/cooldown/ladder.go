// Package cooldown owns the durable state that stops a repeat failure from
// paying for another investigation: the Cooldown ladder, its deterministic
// jitter, the suppression counter, and the settling state of each Fingerprint.
//
// The state lives in the history SQLite file, next to the append-only Page
// history, because it must outlive the one-hour Event TTL, a deleted Page, and a
// server restart. internal/history creates the tables as schema version 3; this
// package only reads and writes rows through the handle history opened, so both
// packages share one write-ahead-log setting and one writer.
package cooldown

import (
	"encoding/hex"
	"errors"
	"time"
)

// DefaultSteps is the Cooldown ladder from the v2 specification: fifteen
// minutes, then thirty, one hour, two hours, four hours, and an eight-hour cap.
// The step index is the backoff level a Page records.
var DefaultSteps = []time.Duration{
	15 * time.Minute,
	30 * time.Minute,
	time.Hour,
	2 * time.Hour,
	4 * time.Hour,
	8 * time.Hour,
}

// DefaultResetAfterHealthy is how long a Subject must be healthy before its
// backoff level returns to the first step.
const DefaultResetAfterHealthy = 30 * time.Minute

// The ladder is rejected at startup rather than at the first failure when it
// cannot bound a flap.
var (
	errLadderNotIncreasing = errors.New("the cooldown ladder must be strictly increasing")
	errLadderTooLong       = errors.New("the cooldown ladder must end at a cap of at most 24 hours")
)

// Ladder is the Cooldown ladder and its healthy-window reset.
type Ladder struct {
	// Steps are the Cooldown durations by level. Level 0 is the first failure.
	Steps []time.Duration
	// ResetAfterHealthy is the healthy observation window that resets the level.
	ResetAfterHealthy time.Duration
}

// DefaultLadder returns the specified ladder.
func DefaultLadder() Ladder {
	return Ladder{Steps: DefaultSteps, ResetAfterHealthy: DefaultResetAfterHealthy}
}

// MaxLevel is the highest level the ladder applies; later failures stay at the
// cap. An empty ladder uses DefaultSteps, so a zero value is still usable.
func (l Ladder) MaxLevel() int {
	if len(l.Steps) == 0 {
		return len(DefaultSteps) - 1
	}
	return len(l.Steps) - 1
}

// Step returns the base Cooldown for a level, clamped to the cap.
func (l Ladder) Step(level int) time.Duration {
	steps := l.Steps
	if len(steps) == 0 {
		steps = DefaultSteps
	}
	switch {
	case level < 0:
		return steps[0]
	case level >= len(steps):
		return steps[len(steps)-1]
	default:
		return steps[level]
	}
}

// Next returns the level a Page that used level leaves for the next one, capped
// at the last step.
func (l Ladder) Next(level int) int {
	if level < 0 {
		return 0
	}
	if level > l.MaxLevel() {
		return l.MaxLevel()
	}
	return level
}

// CooldownUntil returns the moment a Page that opened at openedAt and used
// level stops suppressing new Pages. The jitter is derived from the
// Fingerprint, so the same failure always produces the same window and a test
// can assert it exactly.
func (l Ladder) CooldownUntil(level int, fingerprint string, openedAt time.Time) time.Time {
	return openedAt.Add(Jitter(l.Step(level), fingerprint))
}

// validate reports whether the ladder is strictly increasing and finite. The
// Helm chart enforces the same rule, so a bad value fails at startup rather
// than at the first failure.
func (l Ladder) validate() error {
	if len(l.Steps) == 0 {
		return nil
	}
	previous := time.Duration(0)
	for _, step := range l.Steps {
		if step <= previous {
			return errLadderNotIncreasing
		}
		previous = step
	}
	if previous > 24*time.Hour {
		return errLadderTooLong
	}
	return nil
}

// Jitter applies the deterministic plus-or-minus ten percent window from the
// first four hexadecimal digits of the Fingerprint: a value in [0, 20] maps to
// [-10 %, +10 %] in one-percent steps. Determinism keeps retries and tests
// reproducible. A value that is not hex jitters by zero rather than failing,
// because jitter must never block a Cooldown.
func Jitter(base time.Duration, fingerprint string) time.Duration {
	if base <= 0 || len(fingerprint) < 4 {
		return base
	}
	digits, err := hex.DecodeString(fingerprint[:4])
	if err != nil {
		return base
	}
	offset := int(digits[0])<<8 | int(digits[1])
	percent := offset%21 - 10
	return base + base*time.Duration(percent)/100
}

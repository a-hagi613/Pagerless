package cooldown

import (
	"testing"
	"time"
)

// testFingerprint is a 64-character lowercase fingerprint. The first four hex
// digits are "0014", which makes the jitter offset twenty and therefore +10 %.
const testFingerprint = "0014" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

func TestDefaultLadderIsTheSpecifiedOne(t *testing.T) {
	t.Parallel()
	want := []time.Duration{
		15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
	}
	ladder := DefaultLadder()
	if len(ladder.Steps) != len(want) {
		t.Fatalf("ladder has %d steps, want %d", len(ladder.Steps), len(want))
	}
	for index, step := range want {
		if ladder.Steps[index] != step {
			t.Errorf("step %d = %s, want %s", index, ladder.Steps[index], step)
		}
	}
	if ladder.ResetAfterHealthy != 30*time.Minute {
		t.Errorf("reset window = %s, want 30m", ladder.ResetAfterHealthy)
	}
	if ladder.MaxLevel() != 5 {
		t.Errorf("max level = %d, want 5", ladder.MaxLevel())
	}
}

func TestStepClampsToTheCap(t *testing.T) {
	t.Parallel()
	ladder := DefaultLadder()
	if got := ladder.Step(-1); got != 15*time.Minute {
		t.Errorf("step below zero = %s, want the first step", got)
	}
	if got := ladder.Step(99); got != 8*time.Hour {
		t.Errorf("step past the cap = %s, want the eight-hour cap", got)
	}
	// A zero value still behaves as the specified ladder.
	var zero Ladder
	if got := zero.Step(0); got != 15*time.Minute {
		t.Errorf("zero ladder step = %s, want 15m", got)
	}
}

func TestNextNeverExceedsTheCap(t *testing.T) {
	t.Parallel()
	ladder := DefaultLadder()
	for level, want := range map[int]int{-3: 0, 0: 0, 3: 3, 5: 5, 9: 5} {
		if got := ladder.Next(level); got != want {
			t.Errorf("Next(%d) = %d, want %d", level, got, want)
		}
	}
}

// TestJitterIsDeterministic pins the two extremes: the offset is one percent per
// unit over [0, 20], so "0000" is -10 % and "0014" (twenty) is +10 %.
func TestJitterIsDeterministic(t *testing.T) {
	t.Parallel()
	base := 15 * time.Minute
	if got := Jitter(base, "0000"+testFingerprint[4:]); got != base-base/10 {
		t.Errorf("jitter of 0000 = %s, want %s", got, base-base/10)
	}
	if got := Jitter(base, testFingerprint); got != base+base/10 {
		t.Errorf("jitter of 0014 = %s, want %s", got, base+base/10)
	}
	for _, fingerprint := range []string{testFingerprint, "abcd" + testFingerprint[4:], "ffff" + testFingerprint[4:]} {
		first := Jitter(base, fingerprint)
		if again := Jitter(base, fingerprint); again != first {
			t.Errorf("jitter of %s is not deterministic: %s != %s", fingerprint, again, first)
		}
		if first < base-base/10 || first > base+base/10 {
			t.Errorf("jitter of %s = %s, outside plus or minus ten percent of %s", fingerprint, first, base)
		}
	}
	// A fingerprint too short or not hex jitters by zero rather than failing.
	for _, fingerprint := range []string{"", "00", "zzzz" + testFingerprint[4:]} {
		if got := Jitter(base, fingerprint); got != base {
			t.Errorf("jitter of %q = %s, want the base %s", fingerprint, got, base)
		}
	}
}

func TestCooldownUntilUsesTheLevelStepAndTheJitter(t *testing.T) {
	t.Parallel()
	ladder := DefaultLadder()
	opened := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	want := opened.Add(Jitter(ladder.Step(0), testFingerprint))
	if got := ladder.CooldownUntil(0, testFingerprint, opened); !got.Equal(want) {
		t.Errorf("CooldownUntil = %s, want %s", got, want)
	}
}

func TestValidateRejectsAnUnusableLadder(t *testing.T) {
	t.Parallel()
	cases := map[string]Ladder{
		"decreasing": {Steps: []time.Duration{time.Hour, time.Minute}},
		"repeated":   {Steps: []time.Duration{time.Hour, time.Hour}},
		"too long":   {Steps: []time.Duration{time.Hour, 48 * time.Hour}},
	}
	for name, ladder := range cases {
		if err := ladder.validate(); err == nil {
			t.Errorf("%s: validate accepted an unusable ladder", name)
		}
	}
	if err := DefaultLadder().validate(); err != nil {
		t.Errorf("the default ladder must validate: %v", err)
	}
	if err := (Ladder{}).validate(); err != nil {
		t.Errorf("an empty ladder uses the default steps and must validate: %v", err)
	}
}

func TestNewRejectsAnUnusableLadder(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if _, err := New(store.db, WithLadder(Ladder{Steps: []time.Duration{time.Minute, time.Second}})); err == nil {
		t.Fatal("New accepted a decreasing ladder")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("New accepted a nil database handle")
	}
}

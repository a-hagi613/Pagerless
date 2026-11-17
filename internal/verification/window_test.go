package verification_test

import (
	"testing"
	"time"

	"github.com/a-hagi613/pagerless/internal/verification"
)

// env returns a getenv function that answers only the named variable.
func env(name, value string) func(string) string {
	return func(key string) string {
		if key == name {
			return value
		}
		return ""
	}
}

func TestWindowFromEnvironmentDefaultsToFiveMinutes(t *testing.T) {
	t.Parallel()
	window, err := verification.WindowFromEnvironment(env(verification.WindowEnvironment, ""))
	if err != nil {
		t.Fatalf("WindowFromEnvironment() error = %v", err)
	}
	if window != verification.DefaultWindow {
		t.Fatalf("window = %v, want %v", window, verification.DefaultWindow)
	}
	if verification.DefaultWindow != 5*time.Minute {
		t.Fatalf("DefaultWindow = %v, want five minutes per ADR-0006", verification.DefaultWindow)
	}
}

func TestWindowFromEnvironmentAcceptsAPositiveDuration(t *testing.T) {
	t.Parallel()
	window, err := verification.WindowFromEnvironment(env(verification.WindowEnvironment, " 60s "))
	if err != nil {
		t.Fatalf("WindowFromEnvironment() error = %v", err)
	}
	if window != time.Minute {
		t.Fatalf("window = %v, want %v", window, time.Minute)
	}
}

func TestWindowFromEnvironmentRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"soon", "0s", "-30s"} {
		if _, err := verification.WindowFromEnvironment(env(verification.WindowEnvironment, value)); err == nil {
			t.Errorf("WindowFromEnvironment(%q) accepted an invalid window", value)
		}
	}
}

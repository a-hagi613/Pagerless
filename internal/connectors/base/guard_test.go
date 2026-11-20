package base_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/a-hagi613/pagerless/internal/connectors/base"
)

// TestADuplicateMetricRegistrationIsAnError keeps a second Metrics value on one
// registry from silently adopting or panicking: the collision names both
// descriptors and is returned to the startup path.
func TestADuplicateMetricRegistrationIsAnError(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	if _, err := base.NewMetrics(registry); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := base.NewMetrics(registry)
	if err == nil {
		t.Fatal("the second registration succeeded, want a collision error")
	}
	message := err.Error()
	for _, want := range []string{"collides", "existing", "new", base.MetricRequests} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want it to mention %q", message, want)
		}
	}
	if count := strings.Count(message, base.MetricRequests); count < 2 {
		t.Errorf("error = %q, want both colliding descriptors named (%s appears %d times)",
			message, base.MetricRequests, count)
	}
}

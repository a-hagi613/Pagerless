package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPatchBoundsConfiguration(t *testing.T) {
	const cpuMinimum, memoryMinimum, memoryMaximum = "250m", "32Mi", "1Gi"
	bounds, err := parsePatchBounds(cpuMinimum, "2", memoryMinimum, memoryMaximum)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name     corev1.ResourceName
		min, max string
	}{
		{corev1.ResourceCPU, cpuMinimum, "2"},
		{corev1.ResourceMemory, memoryMinimum, memoryMaximum},
	} {
		got := bounds[check.name]
		if got.Min.Cmp(resource.MustParse(check.min)) != 0 || got.Max.Cmp(resource.MustParse(check.max)) != 0 {
			t.Errorf("%s bounds = %s..%s, want %s..%s", check.name, got.Min.String(), got.Max.String(), check.min, check.max)
		}
	}
	for _, inputs := range [][4]string{
		{"invalid", "2", memoryMinimum, memoryMaximum},
		{"0", "2", memoryMinimum, memoryMaximum},
		{cpuMinimum, "100m", memoryMinimum, memoryMaximum},
		{cpuMinimum, "2", "-32Mi", memoryMaximum},
		{cpuMinimum, "2", "2Gi", memoryMaximum},
	} {
		_, err := parsePatchBounds(inputs[0], inputs[1], inputs[2], inputs[3])
		if err == nil || !strings.Contains(err.Error(), "bound") {
			t.Errorf("bounds %v: error = %v, want invalid bounds", inputs, err)
		}
	}
}

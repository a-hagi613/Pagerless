package v1alpha1

import (
	"slices"
	"testing"
)

func TestDependsOnPathsReadsTheAnnotation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		want        []string
	}{
		{name: "no annotations", want: nil},
		{name: "another annotation", annotations: map[string]string{AnnotationSourcePath: "apps/a.yaml"}, want: nil},
		{
			name:        "one path",
			annotations: map[string]string{AnnotationDependsOnPaths: "shared/config.yaml"},
			want:        []string{"shared/config.yaml"},
		},
		{
			name:        "several paths in sorted order",
			annotations: map[string]string{AnnotationDependsOnPaths: "shared/b.yaml, shared/a.yaml"},
			want:        []string{"shared/a.yaml", "shared/b.yaml"},
		},
		{
			name:        "duplicates and empty entries",
			annotations: map[string]string{AnnotationDependsOnPaths: "shared/a.yaml, ,shared/a.yaml,"},
			want:        []string{"shared/a.yaml"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DependsOnPaths(tc.annotations); !slices.Equal(got, tc.want) {
				t.Errorf("DependsOnPaths(%v) = %v, want %v", tc.annotations, got, tc.want)
			}
		})
	}
}

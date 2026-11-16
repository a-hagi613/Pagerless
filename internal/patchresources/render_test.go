package patchresources

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

func list(pairs ...string) corev1.ResourceList {
	l := corev1.ResourceList{}
	for i := 0; i < len(pairs); i += 2 {
		l[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return l
}

var checkout = v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindDeployment, Namespace: "sandbox", Name: "checkout"}

const header = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: sandbox
spec:
  template:
    spec:
      containers:
`

func TestRenderChangesOnlyTheResourcesBlock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		src      string
		workload v1alpha1.WorkloadReference
		requests corev1.ResourceList
		limits   corev1.ResourceList
		want     string
	}{
		{
			name: "adds a block after the image and keeps comments",
			src: header + `        # the checkout service
        - name: app
          image: registry.invalid/checkout:1.0 # pinned
          args: ["--port", "8080"]
`,
			workload: checkout,
			limits:   list("memory", "512Mi"),
			want: header + `        # the checkout service
        - name: app
          image: registry.invalid/checkout:1.0 # pinned
          resources:
            limits:
              memory: 512Mi
          args: ["--port", "8080"]
`,
		},
		{
			name: "replaces one limit and keeps the other entries",
			src: header + `        - name: sidecar
          image: registry.invalid/sidecar:1.0
        - name: app
          image: registry.invalid/checkout:1.0
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 256Mi
          env:
            - name: MODE
              value: prod
`,
			workload: checkout,
			limits:   list("memory", "1Gi"),
			want: header + `        - name: sidecar
          image: registry.invalid/sidecar:1.0
        - name: app
          image: registry.invalid/checkout:1.0
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 1Gi
          env:
            - name: MODE
              value: prod
`,
		},
		{
			name: "rewrites an empty flow-style block",
			src: header + `        - name: app
          image: registry.invalid/checkout:1.0
          resources: {}
`,
			workload: checkout,
			requests: list("cpu", "250m"),
			want: header + `        - name: app
          image: registry.invalid/checkout:1.0
          resources:
            requests:
              cpu: 250m
`,
		},
		{
			name: "follows the file's own indentation",
			src: `apiVersion: apps/v1
kind: StatefulSet
metadata:
    name: db
spec:
    template:
        spec:
            containers:
            -   name: db
                image: registry.invalid/db:1.0
`,
			workload: v1alpha1.WorkloadReference{Kind: v1alpha1.WorkloadKindStatefulSet, Namespace: "sandbox", Name: "db"},
			limits:   list("memory", "2Gi"),
			want: `apiVersion: apps/v1
kind: StatefulSet
metadata:
    name: db
spec:
    template:
        spec:
            containers:
            -   name: db
                image: registry.invalid/db:1.0
                resources:
                  limits:
                    memory: 2Gi
`,
		},
		{
			name: "edits the one document of several that defines the Workload",
			src: `apiVersion: v1
kind: Service
metadata:
  name: checkout
---
` + header + `        - name: app
          image: registry.invalid/checkout:1.0
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cart
spec:
  template:
    spec:
      containers:
        - name: app
          image: registry.invalid/cart:1.0
`,
			workload: checkout,
			limits:   list("memory", "512Mi"),
			want: `apiVersion: v1
kind: Service
metadata:
  name: checkout
---
` + header + `        - name: app
          image: registry.invalid/checkout:1.0
          resources:
            limits:
              memory: 512Mi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cart
spec:
  template:
    spec:
      containers:
        - name: app
          image: registry.invalid/cart:1.0
`,
		},
		{
			name:     "handles a file without a final newline",
			src:      header + "        - name: app\n          image: registry.invalid/checkout:1.0",
			workload: checkout,
			limits:   list("memory", "512Mi"),
			want: header + `        - name: app
          image: registry.invalid/checkout:1.0
          resources:
            limits:
              memory: 512Mi
`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := render([]byte(tc.src), tc.workload, containerApp(tc.workload), tc.requests, tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("rendered file:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func containerApp(w v1alpha1.WorkloadReference) string {
	if w.Name == "db" {
		return "db"
	}
	return appName
}

const (
	appName      = "app"
	appContainer = "        - name: app\n          image: x\n"
)

func TestRenderRefusesManifestsItCannotEditSafely(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		src       string
		container string
		wantErr   string
		unsafe    bool
	}{
		{"not YAML", "a: [unclosed\n", "app", "not valid YAML", false},
		{"no such Workload", "apiVersion: v1\nkind: Service\nmetadata:\n  name: checkout\n", "app", "does not define", false},
		{"no such container", header + appContainer, "worker", "no container named worker", false},
		{"defined twice", header + appContainer + "---\n" + header + appContainer, appName, "more than once", true},
		{"carriage returns", strings.ReplaceAll(header+appContainer, "\n", "\r\n"), appName, "carriage returns", true},
		{"flow-style container", header + "        - {name: app, image: x}\n", appName, "flow style", true},
		{"resource claims", header + appContainer + "          resources:\n            claims:\n              - name: gpu\n",
			appName, "claims", true},
		{"unknown resources field", header + appContainer + "          resources:\n            burst: 1\n",
			appName, "cannot be read", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := render([]byte(tc.src), checkout, tc.container, nil, list("memory", "512Mi"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if tc.unsafe && !errors.Is(err, errUnsupported) {
				t.Errorf("error = %v, want errUnsupported", err)
			}
		})
	}
}

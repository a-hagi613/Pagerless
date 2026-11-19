package scale

import (
	"strings"
	"testing"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
  namespace: sandbox
spec:
  replicas: 1
  selector:
    matchLabels:
      app: checkout
  template:
    metadata:
      labels:
        app: checkout
    spec:
      containers:
        - name: app
          image: registry.invalid/checkout:1.0
`

var checkout = v1alpha1.TargetReference{
	Kind: v1alpha1.TargetKindDeployment, Namespace: "sandbox", Name: "checkout",
}

func TestRenderChangesOnlyTheReplicaCount(t *testing.T) {
	rendered, err := render([]byte(deployment), checkout, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(deployment, "  replicas: 1\n", "  replicas: 3\n", 1); string(rendered) != want {
		t.Errorf("rendered file =\n%s\nwant\n%s", rendered, want)
	}
	if strings.Contains(string(rendered), "replicas: 1") {
		t.Errorf("the rendered file still declares the old count:\n%s", rendered)
	}
}

func TestRenderKeepsATrailingComment(t *testing.T) {
	src := strings.Replace(deployment, "  replicas: 1\n", "  replicas: 1 # keep me\n", 1)
	rendered, err := render([]byte(src), checkout, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "  replicas: 2 # keep me\n") {
		t.Errorf("the rendered file lost the comment or the count:\n%s", rendered)
	}
}

func TestRenderInsertsAMissingReplicaCount(t *testing.T) {
	src := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: cart
  namespace: sandbox
spec:
  serviceName: cart
`
	rendered, err := render([]byte(src), v1alpha1.TargetReference{
		Kind: v1alpha1.TargetKindStatefulSet, Namespace: "sandbox", Name: "cart",
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "spec:\n  replicas: 4\n  serviceName: cart\n") {
		t.Errorf("the rendered file does not declare the replica count after spec:\n%s", rendered)
	}
}

func TestRenderLeavesTheFileAloneWhenTheCountIsUnchanged(t *testing.T) {
	rendered, err := render([]byte(deployment), checkout, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(rendered) != deployment {
		t.Errorf("rendered file =\n%s\nwant the original", rendered)
	}
}

func TestRenderRefusesFilesItCannotEditSafely(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"carriage returns", strings.ReplaceAll(deployment, "\n", "\r\n"), "carriage returns"},
		{"another Target", strings.ReplaceAll(deployment, "name: checkout", "name: cart"), "does not define"},
		{"no spec", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: checkout\n", "no spec mapping"},
		{"replicas is a mapping", strings.Replace(deployment, "  replicas: 1\n",
			"  replicas:\n    count: 1\n", 1), "not a plain scalar"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := render([]byte(tc.src), checkout, 2)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

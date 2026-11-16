package testenv

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Table returns what `kubectl get` receives for a resource of the pagerless.dev
// group in the harness namespace: the server-side table with its printer columns.
func (e *Env) Table(t testing.TB, resource string) *metav1.Table {
	t.Helper()
	raw, err := e.Clientset.Discovery().RESTClient().Get().
		AbsPath("/apis/pagerless.dev/v1alpha1/namespaces/"+Namespace+"/"+resource).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io").
		DoRaw(t.Context())
	if err != nil {
		t.Fatalf("get the table of %s: %v", resource, err)
	}
	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("decode the table of %s: %v", resource, err)
	}
	return &table
}

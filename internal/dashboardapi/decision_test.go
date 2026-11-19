package dashboardapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
)

// stalePageReader serves one Page as it was before another writer changed it, which is what the
// manager's cached client returns until its informer catches up.
type stalePageReader struct {
	page *v1alpha1.Page
}

func (r stalePageReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if key.Namespace != r.page.Namespace || key.Name != r.page.Name {
		return apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "pages"}, key.Name)
	}
	r.page.DeepCopyInto(obj.(*v1alpha1.Page))
	return nil
}

func (stalePageReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}

// decisionHandler builds the API over one Page. The cached reader serves stale, and the live reader
// serves the API server's copy, exactly as the server wires them.
func decisionHandler(t *testing.T, page *v1alpha1.Page, cached *v1alpha1.Page) (*dashboardapi.Handler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(page).WithStatusSubresource(page).Build()
	return dashboardapi.New(dashboardapi.Config{
		Namespace: testNamespace,
		Reader:    stalePageReader{page: cached},
		Live:      cl,
		Writer:    cl,
		Auth:      fakeAuth{},
		Broker:    dashboardapi.NewBroker(),
	}), cl
}

// decisionPage returns a Page that the API server records as Proposed with one hash.
func decisionPage(name, hash string) *v1alpha1.Page {
	meta := metav1.ObjectMeta{Namespace: testNamespace, Name: name}
	return &v1alpha1.Page{
		ObjectMeta: meta,
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindDeployment, Namespace: "shop", Name: "checkout", UID: "uid-checkout",
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent},
		},
		Status: v1alpha1.PageStatus{
			Phase: v1alpha1.PhaseProposed,
			Proposal: &v1alpha1.Proposal{
				Hash:    hash,
				Action:  v1alpha1.ActionScale,
				Attempt: 1,
			},
		},
	}
}

// TestADecisionReadsThePageFromTheAPIServerNotTheCache pins the fix for the 409s that failed
// TestAPortalDecisionAppliesToAnExternallyProposedPage: an operator who patches a Proposal into status
// (the CLI, a script, or the kind parity check) and decides at once must be accepted, even while the
// manager's cache still shows the Page Investigating. The Phase and the hash gate a write, so the read
// must be the API server's copy.
func TestADecisionReadsThePageFromTheAPIServerNotTheCache(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a1", 32)
	page := decisionPage("checkout-proposed", hash)
	cached := page.DeepCopy()
	cached.Status.Phase = v1alpha1.PhaseInvestigating
	cached.Status.Proposal = nil
	handler, cl := decisionHandler(t, page, cached)

	rec, body := requestPath(t, handler, http.MethodPost, "/api/pages/"+page.Name+"/decision",
		fmt.Sprintf(`{"decision":"approve","proposalHash":%q,"reason":"parity check"}`, hash))
	if rec.Code != http.StatusCreated {
		t.Fatalf("decision = %d, want %d: %s", rec.Code, http.StatusCreated, body)
	}
	var decision map[string]string
	if err := json.Unmarshal(body, &decision); err != nil {
		t.Fatalf("decode the decision answer: %v: %s", err, body)
	}
	if decision["decidedBy"] != testUser {
		t.Errorf("decidedBy = %q, want the signed-in user", decision["decidedBy"])
	}

	var stored v1alpha1.Page
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(page), &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[v1alpha1.AnnotationDecision]; got != v1alpha1.DecisionAnnotationApprove {
		t.Errorf("decision annotation = %q, want %q", got, v1alpha1.DecisionAnnotationApprove)
	}
	if got := stored.Annotations[v1alpha1.AnnotationDecisionProposal]; got != hash {
		t.Errorf("decision proposal annotation = %q, want the displayed hash %q", got, hash)
	}
}

// TestADecisionOnAPageThatIsNotProposedIsRefused keeps the refusal for the case it describes: the API
// server's own copy is not Proposed, so no decision can bind to a Proposal the approver never saw.
func TestADecisionOnAPageThatIsNotProposedIsRefused(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("b2", 32)
	page := decisionPage("checkout-investigating", hash)
	page.Status.Phase = v1alpha1.PhaseInvestigating
	page.Status.Proposal = nil
	handler, _ := decisionHandler(t, page, page.DeepCopy())

	rec, body := requestPath(t, handler, http.MethodPost, "/api/pages/"+page.Name+"/decision",
		fmt.Sprintf(`{"decision":"approve","proposalHash":%q}`, hash))
	if rec.Code != http.StatusConflict {
		t.Fatalf("decision = %d, want %d: %s", rec.Code, http.StatusConflict, body)
	}
	if !strings.Contains(string(body), "page_not_proposed") {
		t.Errorf("body = %s, want the page_not_proposed code", body)
	}
}

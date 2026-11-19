package dashboardapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/dashboardapi"
	"github.com/a-hagi613/pagerless/internal/history"
)

// newListHandler builds the API over one Page so a list and a detail read can be asserted without a
// cluster. The history store is empty; the live resource is the only row.
func newListHandler(t *testing.T, page *v1alpha1.Page) *dashboardapi.Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(page).WithStatusSubresource(page).Build()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	return dashboardapi.New(dashboardapi.Config{
		Namespace: testNamespace,
		Reader:    cl,
		Writer:    cl,
		Auth:      fakeAuth{},
		History:   store,
		Broker:    dashboardapi.NewBroker(),
		Version:   "2.0.0",
	})
}

// TestTheListCarriesTheSeverityTheProposalHashAndThePullRequest keeps the inbox from needing a detail read
// per row: the three fields the card renders come with the summary.
func TestTheListCarriesTheSeverityTheProposalHashAndThePullRequest(t *testing.T) {
	t.Parallel()
	summaryMeta := metav1.ObjectMeta{Namespace: testNamespace, Name: "checkout-7d9f"}
	page := &v1alpha1.Page{
		ObjectMeta: summaryMeta,
		Spec: v1alpha1.PageSpec{
			Target: v1alpha1.TargetReference{
				Kind: v1alpha1.TargetKindDeployment, Namespace: "shop", Name: "checkout", UID: "uid-checkout",
			},
			Trigger: v1alpha1.Trigger{Source: v1alpha1.TriggerSourceEvent},
		},
		Status: v1alpha1.PageStatus{
			Phase:    v1alpha1.PhaseProposed,
			Severity: v1alpha1.SeverityCritical,
			Proposal: &v1alpha1.Proposal{
				Hash:    "5f2c1d0e9b8a7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d",
				Action:  v1alpha1.ActionScale,
				Attempt: 1,
			},
			PullRequest: &v1alpha1.PullRequest{
				Number: 12,
				URL:    "https://github.example/acme/shop/pull/12",
				State:  v1alpha1.PullRequestOpen,
			},
		},
	}
	handler := newListHandler(t, page)

	req := httptest.NewRequest(http.MethodGet, "/api/pages", nil)
	req.AddCookie(&http.Cookie{Name: testSession, Value: testSigned})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/pages = %d: %s", rec.Code, rec.Body.String())
	}
	var list dashboardapi.ListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Pages) != 1 {
		t.Fatalf("list = %+v, want one Page", list.Pages)
	}
	row := list.Pages[0]
	if row.Severity != v1alpha1.SeverityCritical {
		t.Errorf("severity = %q, want critical", row.Severity)
	}
	if row.ProposalHash != page.Status.Proposal.Hash {
		t.Errorf("proposalHash = %q, want the displayed hash", row.ProposalHash)
	}
	if row.PullRequest == nil || row.PullRequest.Number != 12 {
		t.Errorf("pullRequest = %+v, want PR #12", row.PullRequest)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/pages/checkout-7d9f", nil)
	req.AddCookie(&http.Cookie{Name: testSession, Value: testSigned})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the record = %d: %s", rec.Code, rec.Body.String())
	}
	var detail dashboardapi.DetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Severity != v1alpha1.SeverityCritical {
		t.Errorf("detail severity = %q, want critical", detail.Severity)
	}
}

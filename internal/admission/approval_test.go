package admission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
)

const (
	serverUser = "system:serviceaccount:pagerless:pagerless-server"
	aliceUser  = "alice"
	bobUser    = "bob"
)

// stubApprovers answers the group question from a set, or fails.
type stubApprovers struct {
	members map[string]bool
	err     error
}

func (s stubApprovers) IsApprover(_ context.Context, username string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.members[username], nil
}

func approvalFor(approver string) *v1alpha1.TriageApproval {
	approval := &v1alpha1.TriageApproval{
		Spec: v1alpha1.TriageApprovalSpec{
			Triage:       "triage-checkout-abc",
			ProposalHash: strings.Repeat("a", 64),
			Decision:     v1alpha1.DecisionApproved,
			Approver:     approver,
		},
	}
	approval.Name = "triage-checkout-abc-0123456789abcdef"
	approval.Namespace = "pagerless"
	return approval
}

// stamp runs the mutating webhook and returns the object the API server would store.
func stamp(t *testing.T, requester, approver string) *v1alpha1.TriageApproval {
	t.Helper()
	original := approvalFor(approver)
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	response := ApprovalStamper{ServerUsername: serverUser}.Handle(t.Context(), webhookadmission.Request{
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: requester},
	})
	if !response.Allowed {
		t.Fatalf("stamping was refused: %+v", response.Result)
	}
	if len(response.Patches) == 0 {
		return original
	}
	rawPatch, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatalf("encode the patch: %v", err)
	}
	patch, err := jsonpatch.DecodePatch(rawPatch)
	if err != nil {
		t.Fatalf("decode the patch: %v", err)
	}
	patched, err := patch.Apply(raw)
	if err != nil {
		t.Fatalf("apply the patch: %v", err)
	}
	var stored v1alpha1.TriageApproval
	if err := json.Unmarshal(patched, &stored); err != nil {
		t.Fatalf("decode the stored object: %v", err)
	}
	return &stored
}

func TestStamperRecordsTheSignedInIdentityForEveryoneButTheServer(t *testing.T) {
	t.Parallel()
	if got := stamp(t, aliceUser, "").Spec.Approver; got != aliceUser {
		t.Errorf("stamped approver = %q, want the requester %q", got, aliceUser)
	}
	if got := stamp(t, aliceUser, bobUser).Spec.Approver; got != aliceUser {
		t.Errorf("a client-supplied approver became %q, want it overwritten with %q", got, aliceUser)
	}
	if got := stamp(t, serverUser, aliceUser).Spec.Approver; got != aliceUser {
		t.Errorf("asserted approver = %q, want the dashboard's %q kept", got, aliceUser)
	}
}

func TestStamperRefusesAnUnidentifiedRequestOrAnEmptyAssertion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		requester string
		approver  string
	}{
		{name: "no identity", requester: "", approver: aliceUser},
		{name: "an empty assertion", requester: serverUser, approver: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(approvalFor(test.approver))
			if err != nil {
				t.Fatal(err)
			}
			response := ApprovalStamper{ServerUsername: serverUser}.Handle(t.Context(), webhookadmission.Request{
				Operation: admissionv1.Create,
				Object:    runtime.RawExtension{Raw: raw},
				UserInfo:  authenticationv1.UserInfo{Username: test.requester},
			})
			if response.Allowed {
				t.Error("the request was admitted, want a refusal")
			}
		})
	}
}

func TestApproverRule(t *testing.T) {
	t.Parallel()
	members := stubApprovers{members: map[string]bool{aliceUser: true}}
	for _, test := range []struct {
		name      string
		requester string
		approver  string
		approvers ApproverGroups
		want      bool
		wantText  string
	}{
		{
			name: "a user approving as themselves", requester: aliceUser, approver: aliceUser,
			approvers: members, want: true,
		},
		{
			name: "a user asserting another user", requester: aliceUser, approver: bobUser,
			approvers: members, wantText: "only pagerless-server may assert",
		},
		{
			name: "the server asserting an approver", requester: serverUser, approver: aliceUser,
			approvers: members, want: true,
		},
		{
			name: "the server asserting a non-member", requester: serverUser, approver: bobUser,
			approvers: members, wantText: "not a member",
		},
		{
			name: "the server asserting itself", requester: serverUser, approver: serverUser,
			approvers: members, wantText: "not itself",
		},
		{
			name: "the server with no group check", requester: serverUser, approver: aliceUser,
			approvers: nil, wantText: "no Keycloak approver-group check",
		},
		{
			name: "Keycloak cannot answer", requester: serverUser, approver: aliceUser,
			approvers: stubApprovers{err: errors.New("unavailable")}, wantText: "may approve",
		},
		{
			name: "an empty approver", requester: aliceUser, approver: "",
			approvers: members, wantText: "empty",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := &ApprovalValidator{ServerUsername: serverUser, Approvers: test.approvers}
			response := validator.checkApprover(t.Context(), test.requester, test.approver)
			if test.want {
				if response != nil {
					t.Fatalf("refused: %+v", response.Result)
				}
				return
			}
			if response == nil {
				t.Fatal("the approver was accepted, want a refusal")
			}
			message := ""
			if response.Result != nil {
				message = response.Result.Message
			}
			if !strings.Contains(message, test.wantText) {
				t.Errorf("refusal message = %q, want it to mention %q", message, test.wantText)
			}
		})
	}
}

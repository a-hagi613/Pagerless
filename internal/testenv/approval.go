package testenv

import (
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/server"
)

// ApproverRole is the Role in the harness namespace that grants what an
// approver needs: reading Triages and creating TriageApprovals.
const ApproverRole = "pagerless-approver"

// ClientAs returns a client that the API server authenticates as the user, as
// a browser or kubectl context authenticated as that user would be. The user
// holds only the permissions that RBAC grants it, so a test must call
// GrantApprover for a user who is to create TriageApprovals.
func (e *Env) ClientAs(t testing.TB, user string) client.Client {
	t.Helper()
	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(e.asUser(user), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create a client for %s: %v", user, err)
	}
	return c
}

func (e *Env) asUser(user string) *rest.Config {
	cfg := rest.CopyConfig(e.Config)
	cfg.QPS, cfg.Burst = -1, 0
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	return cfg
}

// GrantApprover binds the pagerless-approver Role to the user, and waits until
// the authorizer lets that user create a TriageApproval.
func (e *Env) GrantApprover(t testing.TB, user string) {
	t.Helper()
	binding := &rbacv1.RoleBinding{
		GenerateName: "approver-", Namespace: Namespace,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ApproverRole},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	if err := e.Client.Create(t.Context(), binding); err != nil {
		t.Fatalf("bind %s to %s: %v", ApproverRole, user, err)
	}
	e.waitUntilCan(t, user, "create", "pagerless.dev", "triageapprovals", "")
}

// GrantReader lets the user read Triages and TriageApprovals in the harness
// namespace, and nothing else.
func (e *Env) GrantReader(t testing.TB, user string) {
	t.Helper()
	role := &rbacv1.Role{
		GenerateName: "reader-", Namespace: Namespace,
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"pagerless.dev"},
			Resources: []string{"triages", "triageapprovals"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
	if err := e.Client.Create(t.Context(), role); err != nil {
		t.Fatalf("create a reader Role: %v", err)
	}
	binding := &rbacv1.RoleBinding{
		GenerateName: "reader-", Namespace: Namespace,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: user}},
	}
	if err := e.Client.Create(t.Context(), binding); err != nil {
		t.Fatalf("bind a reader Role to %s: %v", user, err)
	}
	e.waitUntilCan(t, user, "get", "pagerless.dev", "triages", "")
}

// KubeconfigAs writes a kubeconfig file whose only context authenticates as
// the user, and returns its path. The kubeconfig points at the test API server.
func (e *Env) KubeconfigAs(t testing.TB, user string) string {
	t.Helper()
	const name = "envtest"
	cfg := e.asUser(user)
	raw := clientcmdapi.NewConfig()
	raw.Clusters[name] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	raw.AuthInfos[name] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
		Impersonate:           user,
	}
	raw.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	raw.CurrentContext = name
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*raw, path); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}
	return path
}

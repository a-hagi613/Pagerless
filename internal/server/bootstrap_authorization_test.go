package server_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
)

func authorizationKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	const context = "kind-pagerless"
	raw := clientcmdapi.NewConfig()
	raw.Clusters[context] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	raw.AuthInfos[context] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData,
		Impersonate: cfg.Impersonate.UserName, ImpersonateGroups: cfg.Impersonate.Groups,
	}
	raw.Contexts[context] = &clientcmdapi.Context{Cluster: context, AuthInfo: context}
	raw.CurrentContext = context
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*raw, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func runAuthorizationBootstrap(t *testing.T, config, action string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "uv", "run", "--no-project", "--python", "3.14.10",
		"../../hack/provision-authorization.py", action)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+config, "KIND_CLUSTER_NAME=pagerless")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestAuthorizationBootstrapReusesSecretsAndBindsOnlyTheCurrentUser(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	config := authorizationKubeconfig(t, env.Config)
	for range 2 {
		if output, err := runAuthorizationBootstrap(t, config, "alerts"); err != nil {
			t.Fatalf("provision alert tokens: %v: %s", err, output)
		}
	}
	var receiver, sender corev1.Secret
	receiverKey := client.ObjectKey{Namespace: testenv.Namespace, Name: "pagerless-alertmanager"}
	if err := env.Client.Get(t.Context(), receiverKey, &receiver); err != nil {
		t.Fatal(err)
	}
	senderKey := client.ObjectKey{Namespace: "monitoring", Name: receiver.Name}
	if err := env.Client.Get(t.Context(), senderKey, &sender); err != nil {
		t.Fatal(err)
	}
	if len(receiver.Data["token"]) < 32 || !slices.Equal(receiver.Data["token"], sender.Data["token"]) {
		t.Fatal("bootstrap did not provision matching nonempty random alert credentials")
	}
	initial := slices.Clone(receiver.Data["token"])
	if output, err := runAuthorizationBootstrap(t, config, "alerts"); err != nil {
		t.Fatalf("repeat alert provisioning: %v: %s", err, output)
	} else if strings.Contains(output, string(initial)) {
		t.Fatal("alert provisioning printed its bearer token")
	}
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(&receiver), &receiver); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(initial, receiver.Data["token"]) {
		t.Fatal("repeat bootstrap rotated an existing alert token unexpectedly")
	}
	identity, err := env.Clientset.AuthenticationV1().SelfSubjectReviews().Create(t.Context(),
		&authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	username := identity.Status.UserInfo.Username
	for range 2 {
		if output, err := runAuthorizationBootstrap(t, config, "approver"); err != nil {
			t.Fatalf("provision explicit approver: %v: %s", err, output)
		}
	}
	var all rbacv1.RoleBindingList
	if err := env.Client.List(t.Context(), &all, client.InNamespace(testenv.Namespace)); err != nil {
		t.Fatal(err)
	}
	var bindings []rbacv1.RoleBinding
	for _, candidate := range all.Items {
		if candidate.RoleRef.Name == testenv.ApproverRole {
			bindings = append(bindings, candidate)
		}
	}
	if len(bindings) != 1 {
		t.Fatalf("repeat bootstrap created %d approver bindings, want exactly one", len(bindings))
	}
	binding := bindings[0]
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != rbacv1.UserKind || binding.Subjects[0].Name != username ||
		binding.RoleRef.Name != testenv.ApproverRole {
		t.Errorf("bootstrap approver binding = %+v, want only the explicit current user", binding)
	}
	requireBoundedApproverBootstrap(t, env, config, binding)
}

func requireBoundedApproverBootstrap(t *testing.T, env *testenv.Env, config string, binding rbacv1.RoleBinding) {
	t.Helper()
	for _, account := range []string{"pagerless-agent", server.DefaultServerServiceAccount, "pagerless-executor"} {
		user := "system:serviceaccount:pagerless:" + account
		groups := []string{"system:authenticated", "system:serviceaccounts"}
		if env.Can(t, user, groups, "create", "pagerless.dev", "triageapprovals", "") {
			t.Errorf("bootstrap enabled %s to create Approvals", user)
		}
	}
	// Existing user-managed bindings are never overwritten, even if they reuse bootstrap's deterministic name.
	binding.Subjects = append(binding.Subjects,
		rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "another-approver"})
	if err := env.Client.Update(t.Context(), &binding); err != nil {
		t.Fatal(err)
	}
	output, err := runAuthorizationBootstrap(t, config, "approver")
	if err == nil || !strings.Contains(output, "refusing to overwrite") {
		t.Fatalf("conflicting existing binding: %v: %s", err, output)
	}
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(&binding), &binding); err != nil {
		t.Fatal(err)
	}
	if len(binding.Subjects) != 2 {
		t.Fatal("bootstrap overwrote an existing user binding")
	}
	output, err = runAuthorizationBootstrap(t, authorizationKubeconfig(t, env.ServerConfig), "approver")
	if err == nil || !strings.Contains(output, "ServiceAccount") {
		t.Fatalf("ServiceAccount bootstrap identity: %v: %s", err, output)
	}
}

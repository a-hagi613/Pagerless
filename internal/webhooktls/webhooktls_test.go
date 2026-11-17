package webhooktls_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/internal/server"
	"github.com/a-hagi613/pagerless/internal/testenv"
	"github.com/a-hagi613/pagerless/internal/webhooktls"
)

const serverName = "pagerless-server"

func TestProvisionPublishesACertificateAuthorityThatTrustsTheServingPair(t *testing.T) {
	t.Parallel()
	env := testenv.Start(t)
	scheme, err := server.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	// Run as pagerless-server does, so that its RBAC must allow the patch.
	asServer, err := client.New(env.ServerConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	bundle, err := webhooktls.Provision(t.Context(), asServer, webhooktls.Options{
		ServiceName: serverName,
		Namespace:   testenv.Namespace,
		ConfigName:  serverName,
		CertDir:     dir,
		Now:         time.Now(),

		MutatingConfigName: serverName,
	})
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}

	var mutating admissionregistrationv1.MutatingWebhookConfiguration
	if err := env.Client.Get(t.Context(), types.NamespacedName{Name: serverName}, &mutating); err != nil {
		t.Fatal(err)
	}
	if len(mutating.Webhooks) == 0 {
		t.Fatal("the mutating webhook configuration has no webhooks")
	}
	for _, w := range mutating.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, bundle.CACertPEM) {
			t.Errorf("mutating webhook %s: caBundle was not replaced with the generated certificate authority", w.Name)
		}
	}

	var config admissionregistrationv1.ValidatingWebhookConfiguration
	if err := env.Client.Get(t.Context(), types.NamespacedName{Name: serverName}, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Webhooks) == 0 {
		t.Fatal("the webhook configuration has no webhooks")
	}
	for _, w := range config.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, bundle.CACertPEM) {
			t.Errorf("webhook %s: caBundle was not replaced with the generated certificate authority", w.Name)
		}
	}

	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatalf("load the serving pair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle.CACertPEM) {
		t.Fatal("the certificate authority is not valid PEM")
	}
	verify := func(dns string) error {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots:     roots,
			DNSName:   dns,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
	if err := verify("pagerless-server." + testenv.Namespace + ".svc"); err != nil {
		t.Errorf("the serving certificate does not verify for the Service name: %v", err)
	}
	if err := verify("attacker.example"); err == nil {
		t.Error("the serving certificate verifies for an unrelated name")
	}
	if _, err := os.Stat(filepath.Join(dir, "tls.key")); err != nil {
		t.Error(err)
	} else if info, _ := os.Stat(filepath.Join(dir, "tls.key")); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("tls.key mode = %v, want it readable by its owner only", info.Mode().Perm())
	}
}

func TestGenerateRequiresADNSName(t *testing.T) {
	t.Parallel()
	if _, err := webhooktls.Generate(nil, time.Now()); err == nil {
		t.Error("Generate() accepted an empty list of DNS names")
	}
}

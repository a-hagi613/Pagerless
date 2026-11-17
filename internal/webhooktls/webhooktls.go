// Package webhooktls gives pagerless-server a serving certificate for its
// admission webhooks without any other component. At every start the server
// generates a certificate authority and a serving certificate in memory,
// writes the serving pair to a directory, and publishes the certificate
// authority in the caBundle of its ValidatingWebhookConfiguration and
// MutatingWebhookConfiguration.
//
// The private keys exist only in the process and in the directory it writes,
// which is a memory-backed volume. Because both are regenerated at every
// start, the certificates are valid for ten years and need no rotation.
package webhooktls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Validity is how long the generated certificates are valid.
const Validity = 10 * 365 * 24 * time.Hour

// Bundle is a certificate authority and a serving certificate it signed.
type Bundle struct {
	// CACertPEM is the PEM-encoded certificate authority certificate.
	CACertPEM []byte
	// CertPEM is the PEM-encoded serving certificate.
	CertPEM []byte
	// KeyPEM is the PEM-encoded serving private key.
	KeyPEM []byte
}

// Generate creates a certificate authority and a serving certificate for the
// given DNS names, valid from now for Validity.
func Generate(dnsNames []string, now time.Time) (*Bundle, error) {
	if len(dnsNames) == 0 {
		return nil, fmt.Errorf("at least one DNS name is required")
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "pagerless-server webhook CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate serving key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create serving certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal serving key: %w", err)
	}
	return &Bundle{
		CACertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:    pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// serial returns a random 128-bit certificate serial number.
func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(fmt.Sprintf("read random serial number: %v", err))
	}
	return n
}

// WriteServingPair writes tls.crt and tls.key, the file names that the
// controller-runtime webhook server reads, into dir.
func (b *Bundle) WriteServingPair(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), b.CertPEM, 0o600); err != nil {
		return fmt.Errorf("write serving certificate: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), b.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("write serving key: %w", err)
	}
	return nil
}

// InjectCABundle sets the caBundle of every webhook in the named
// ValidatingWebhookConfiguration to the certificate authority, so that the API
// server trusts the serving certificate.
func InjectCABundle(ctx context.Context, c client.Client, configName string, caPEM []byte) error {
	var config admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: configName}, &config); err != nil {
		return fmt.Errorf("get ValidatingWebhookConfiguration %s: %w", configName, err)
	}
	patch := client.MergeFrom(config.DeepCopy())
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caPEM
	}
	if err := c.Patch(ctx, &config, patch); err != nil {
		return fmt.Errorf("patch ValidatingWebhookConfiguration %s: %w", configName, err)
	}
	return nil
}

// InjectMutatingCABundle sets the caBundle of every webhook in the named
// MutatingWebhookConfiguration to the certificate authority.
func InjectMutatingCABundle(ctx context.Context, c client.Client, configName string, caPEM []byte) error {
	var config admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: configName}, &config); err != nil {
		return fmt.Errorf("get MutatingWebhookConfiguration %s: %w", configName, err)
	}
	patch := client.MergeFrom(config.DeepCopy())
	for i := range config.Webhooks {
		config.Webhooks[i].ClientConfig.CABundle = caPEM
	}
	if err := c.Patch(ctx, &config, patch); err != nil {
		return fmt.Errorf("patch MutatingWebhookConfiguration %s: %w", configName, err)
	}
	return nil
}

// Options select what Provision generates and where it publishes it.
type Options struct {
	// ServiceName and Namespace name the Service that fronts the webhook server.
	ServiceName string
	Namespace   string
	// ConfigName is the ValidatingWebhookConfiguration that receives the certificate authority.
	ConfigName string
	// MutatingConfigName is the MutatingWebhookConfiguration that receives the certificate authority.
	// When it is empty, no mutating configuration is updated.
	MutatingConfigName string
	// CertDir receives tls.crt and tls.key.
	CertDir string
	// Now is the time the certificates are valid from.
	Now time.Time
}

// ServiceDNSNames returns the names under which clients reach a Service.
func ServiceDNSNames(service, namespace string) []string {
	return []string{
		service,
		service + "." + namespace,
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

// Provision generates a certificate for the Service, writes the serving pair to
// CertDir, and publishes the certificate authority in the webhook configuration.
// It returns the bundle so that callers can verify what was published.
func Provision(ctx context.Context, c client.Client, o Options) (*Bundle, error) {
	bundle, err := Generate(ServiceDNSNames(o.ServiceName, o.Namespace), o.Now)
	if err != nil {
		return nil, err
	}
	if err := bundle.WriteServingPair(o.CertDir); err != nil {
		return nil, err
	}
	if err := InjectCABundle(ctx, c, o.ConfigName, bundle.CACertPEM); err != nil {
		return nil, err
	}
	if o.MutatingConfigName != "" {
		if err := InjectMutatingCABundle(ctx, c, o.MutatingConfigName, bundle.CACertPEM); err != nil {
			return nil, err
		}
	}
	return bundle, nil
}

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func readCertificate(t *testing.T, filename string) *x509.Certificate {
	t.Helper()
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%s is not a PEM certificate", filepath.Base(filename))
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestGeneratedAuthorityIdentifiesItselfAndSignsTheServer(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := generate(directory); err != nil {
		t.Fatal(err)
	}
	authority := readCertificate(t, filepath.Join(directory, "ca.crt"))
	server := readCertificate(t, filepath.Join(directory, "tls.crt"))
	if !authority.IsCA || !authority.BasicConstraintsValid {
		t.Fatal("the generated authority is not a CA")
	}
	if len(authority.SubjectKeyId) == 0 || len(authority.AuthorityKeyId) == 0 {
		t.Fatal("the authority has no key identifiers; strict TLS clients reject it")
	}
	if !bytes.Equal(authority.SubjectKeyId, authority.AuthorityKeyId) {
		t.Fatal("the self-signed authority must identify its own key")
	}
	if !bytes.Equal(server.AuthorityKeyId, authority.SubjectKeyId) {
		t.Fatal("the server certificate does not point at the authority key")
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	if _, err := server.Verify(x509.VerifyOptions{
		DNSName: "sandbox-adapter.pagerless-e2e.svc",
		Roots:   roots,
	}); err != nil {
		t.Fatalf("the server certificate does not verify against the authority: %v", err)
	}
}

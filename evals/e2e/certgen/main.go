package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: certgen <output-directory>")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "generate E2E adapter certificate:", err)
		os.Exit(1)
	}
}

func generate(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("create CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("create CA serial: %w", err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Pagerless E2E adapter CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		return fmt.Errorf("sign CA certificate: %w", err)
	}
	serverPublic, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("create server key: %w", err)
	}
	serial, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("create server serial: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sandbox-adapter.pagerless-e2e.svc"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		DNSNames: []string{
			"localhost", "sandbox-adapter", "sandbox-adapter.pagerless-e2e.svc",
			"sandbox-adapter.pagerless-e2e.svc.cluster.local",
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, serverPublic, caPrivate)
	if err != nil {
		return fmt.Errorf("sign server certificate: %w", err)
	}
	if err := writePEM(filepath.Join(directory, "ca.crt"), "CERTIFICATE", caDER, 0o600); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(directory, "tls.crt"), "CERTIFICATE", serverDER, 0o600); err != nil {
		return err
	}
	if len(serverPrivate) != ed25519.PrivateKeySize {
		return errors.New("the server key has an invalid size")
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(serverPrivate)
	if err != nil {
		return fmt.Errorf("encode server key: %w", err)
	}
	if err := writePEM(filepath.Join(directory, "tls.key"), "PRIVATE KEY", privateKey, 0o600); err != nil {
		return err
	}
	return nil
}

func writePEM(filename, kind string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(filename), err)
	}
	if err := pem.Encode(file, &pem.Block{Type: kind, Bytes: contents}); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(filename), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(filename), err)
	}
	return nil
}

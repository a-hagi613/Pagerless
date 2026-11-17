package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
)

func tlsConfig(caFile string) (*tls.Config, error) {
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, os.ErrInvalid
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, nil
}

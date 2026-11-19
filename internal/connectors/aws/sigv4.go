// Package aws implements the minimum AWS surface the read-only CloudWatch
// Connectors need: SigV4 request signing and credential resolution from a
// Secret or the process environment. It deliberately exposes no mutation API.
package aws

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Credentials is one AWS access key set.
type Credentials struct {
	// AccessKeyID is the access key id.
	AccessKeyID string
	// SecretAccessKey is the secret access key.
	SecretAccessKey string
	// SessionToken is the optional temporary-session token.
	SessionToken string
}

// Valid reports whether the credentials are complete.
func (c Credentials) Valid() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// Provider returns credentials at call time, so a rotated Secret takes effect.
type Provider interface {
	// Credentials returns the credentials for one call.
	Credentials(ctx context.Context) (Credentials, error)
}

// Static returns a fixed credential set.
type Static struct {
	// Value is the credential set.
	Value Credentials
}

// Credentials returns the fixed credentials.
func (s Static) Credentials(context.Context) (Credentials, error) {
	if !s.Value.Valid() {
		return Credentials{}, errors.New("the configured AWS credentials are incomplete")
	}
	return s.Value, nil
}

// Environment reads credentials from the standard AWS environment variables,
// which the EKS Pod Identity and IRSA projections populate for the container.
type Environment struct{}

// Credentials reads the environment.
func (Environment) Credentials(context.Context) (Credentials, error) {
	value := Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
	if !value.Valid() {
		return Credentials{}, errors.New("no AWS credentials are available: configure a Secret or an instance role")
	}
	return value, nil
}

// Sign signs a request with Signature Version 4. It mutates the request's
// headers and returns an error rather than signing an unsigned request.
func Sign(request *http.Request, body []byte, credentials Credentials, region, service string, now time.Time) error {
	if !credentials.Valid() {
		return errors.New("AWS credentials are incomplete")
	}
	if region == "" || service == "" {
		return errors.New("an AWS region and service are required to sign a request")
	}
	payloadHash := sha256Hex(body)
	request.Header.Set("X-Amz-Date", now.UTC().Format("20060102T150405Z"))
	if credentials.SessionToken != "" {
		request.Header.Set("X-Amz-Security-Token", credentials.SessionToken)
	}
	request.Header.Set("Host", request.URL.Host)
	signedHeaders := []string{"host", "x-amz-date"}
	if credentials.SessionToken != "" {
		signedHeaders = append(signedHeaders, "x-amz-security-token")
	}
	slices.Sort(signedHeaders)
	canonicalHeaders := canonicalHeaders(request, signedHeaders)
	canonicalRequest := strings.Join([]string{
		request.Method,
		canonicalURI(request),
		canonicalQuery(request),
		canonicalHeaders,
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
	date := now.UTC().Format("20060102")
	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		now.UTC().Format("20060102T150405Z"),
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(credentials.SecretAccessKey, date, region, service),
		[]byte(stringToSign)))
	request.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		credentials.AccessKeyID, scope, strings.Join(signedHeaders, ";"), signature))
	return nil
}

// canonicalHeaders renders the signed headers.
func canonicalHeaders(request *http.Request, names []string) string {
	var builder strings.Builder
	for _, name := range names {
		value := request.Header.Get(name)
		if name == "host" {
			value = request.URL.Host
		}
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.TrimSpace(value))
		builder.WriteString("\n")
	}
	return builder.String()
}

// canonicalURI returns the encoded request path.
func canonicalURI(request *http.Request) string {
	if request.URL.EscapedPath() == "" {
		return "/"
	}
	return request.URL.EscapedPath()
}

// canonicalQuery returns the sorted, encoded query string.
func canonicalQuery(request *http.Request) string {
	return request.URL.Query().Encode()
}

// signingKey derives the SigV4 signing key.
func signingKey(secret, date, region, service string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	regionKey := hmacSHA256(dateKey, []byte(region))
	serviceKey := hmacSHA256(regionKey, []byte(service))
	return hmacSHA256(serviceKey, []byte("aws4_request"))
}

// hmacSHA256 computes an HMAC.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// sha256Hex returns the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

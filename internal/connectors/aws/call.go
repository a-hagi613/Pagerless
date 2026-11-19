package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/a-hagi613/pagerless/internal/connectors/contract"
)

// Doer performs a prepared, signed HTTP request. The Connector client
// implements it.
type Doer interface {
	DoRequest(ctx context.Context, request *http.Request) (contract.Response, error)
}

// Caller signs and performs one AWS API call. It is shared by the CloudWatch
// Logs and CloudWatch metrics Connectors.
type Caller struct {
	// Doer performs the signed request through the Connector guard.
	Doer Doer
	// Provider resolves credentials at call time.
	Provider Provider
	// Region is the AWS region.
	Region string
	// Service is the SigV4 service name: logs or monitoring.
	Service string
	// Endpoint is the regional API endpoint.
	Endpoint string
	// Now defaults to time.Now.
	Now func() time.Time
}

// Call performs one signed API call with the given X-Amz-Target.
func (c *Caller) Call(ctx context.Context, target string, body []byte) (contract.Response, error) {
	credentials, err := c.Provider.Credentials(ctx)
	if err != nil {
		return contract.Response{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return contract.Response{}, fmt.Errorf("build the AWS request: %w", err)
	}
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	request.Header.Set("Content-Type", "application/x-amz-json-1.1")
	request.Header.Set("X-Amz-Target", target)
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if err := Sign(request, body, credentials, c.Region, c.Service, now()); err != nil {
		return contract.Response{}, fmt.Errorf("sign the AWS request: %w", err)
	}
	return c.Doer.DoRequest(ctx, request)
}

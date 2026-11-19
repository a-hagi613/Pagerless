// Package reportapi is the HTTP endpoint through which an agent Job posts step
// events and its report to pagerless-server (ADR-0001).
package reportapi

import (
	"github.com/a-hagi613/pagerless/internal/agentauth"
)

// Authenticator verifies that a request comes from the agent Job of one
// particular Page. It is shared with the tool broker, so both halves of the
// agent API trust exactly the same token.
type Authenticator = agentauth.Authenticator

// Caller is an authenticated agent pod.
type Caller = agentauth.Caller

var (
	// ErrUnauthenticated means the caller presented no valid token for this Page.
	ErrUnauthenticated = agentauth.ErrUnauthenticated
	// ErrForbidden means the caller is authenticated but is not the Page's Job.
	ErrForbidden = agentauth.ErrForbidden
)

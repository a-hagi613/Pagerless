// Package executorapi defines the contract between pagerless-server and
// pagerless-executor (ADR-0005, ADR-0014): the server asks the Executor to
// execute one Triage's Proposal by its hash, and the Executor answers with the
// pull request it opened, or with the result of the Action it carried out live.
//
// The server authenticates with a projected service account token whose
// audience is Audience. A request is idempotent per Proposal hash: repeating
// it returns the pull request that the first request opened, with its current
// state, or the pods that the first request restarted.
package executorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Audience is the audience of the projected service account token that the server presents.
const Audience = "pagerless-executor"

// maxResponseSize bounds the answer the client reads.
const maxResponseSize = 64 << 10

// ExecutePath is the path the server posts an execution request to.
func ExecutePath(triage string) string {
	return "/v1/triages/" + url.PathEscape(triage) + "/execute"
}

// Request asks the Executor to execute the Proposal with the hash.
type Request struct {
	// ProposalHash is the hash of the Proposal to execute. The Executor checks
	// that a TriageApproval exists for exactly this hash.
	ProposalHash string `json:"proposalHash"`
}

// PullRequestState is the state of the pull request that delivers a Proposal.
type PullRequestState string

const (
	// PullRequestOpen means the pull request awaits its check and auto-merge.
	PullRequestOpen PullRequestState = "Open"
	// PullRequestMerged means the pull request merged.
	PullRequestMerged PullRequestState = "Merged"
	// PullRequestClosed means the pull request was closed without merging.
	PullRequestClosed PullRequestState = "Closed"
)

// Response is the Executor's answer to a request it carried out. It holds
// either the pull request that delivers the Proposal, or the result of an
// Action that executed live.
type Response struct {
	// PullRequestURL is the pull request that delivers the Proposal.
	PullRequestURL string `json:"pullRequestURL"`
	// State is the pull request's current state.
	State PullRequestState `json:"state"`
	// MergeCommit is the commit that merged the pull request, once merged.
	MergeCommit string `json:"mergeCommit,omitempty"`
	// Live reports that the Action was carried out against the cluster, so the
	// response holds no pull request.
	Live bool `json:"live,omitempty"`
	// RestartedPods lists the pods that a live RestartWorkload deleted, in the
	// order it deleted them.
	RestartedPods []string `json:"restartedPods,omitempty"`
}

// Reason classifies why the Executor refused a request.
type Reason string

// Reasons that the Executor reports.
const (
	// ReasonBadRequest means the request is malformed.
	ReasonBadRequest Reason = "BadRequest"
	// ReasonTriageNotFound means the Triage does not exist.
	ReasonTriageNotFound Reason = "TriageNotFound"
	// ReasonNotApproved means the Triage is not in the Approved phase, or its decision is not an Approval.
	ReasonNotApproved Reason = "NotApproved"
	// ReasonHashMismatch means the requested hash is not the hash of the Triage's current Proposal.
	ReasonHashMismatch Reason = "HashMismatch"
	// ReasonProposalAltered means the stored Proposal no longer hashes to the hash that was approved.
	ReasonProposalAltered Reason = "ProposalAltered"
	// ReasonNoApproval means no valid TriageApproval exists for the exact hash.
	ReasonNoApproval Reason = "NoApproval"
	// ReasonUnsupportedAction means the Executor does not deliver the Proposal's Action by pull request.
	ReasonUnsupportedAction Reason = "UnsupportedAction"
	// ReasonUnsafeChange means the stored change touches something that an approved change may not.
	ReasonUnsafeChange Reason = "UnsafeChange"
	// ReasonPatchDoesNotApply means the stored patch does not apply cleanly to its base commit.
	ReasonPatchDoesNotApply Reason = "PatchDoesNotApply"
	// ReasonUnauthenticated means the request carried no valid token for this audience.
	ReasonUnauthenticated Reason = "Unauthenticated"
	// ReasonForbidden means the caller is authenticated but is not pagerless-server.
	ReasonForbidden Reason = "Forbidden"
)

// ErrorResponse is the body of every non-success answer.
type ErrorResponse struct {
	// Reason classifies the failure.
	Reason Reason `json:"reason"`
	// Message describes the failure.
	Message string `json:"message"`
}

// Error is a non-success answer from the Executor.
type Error struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Reason classifies the failure, when the Executor sent one.
	Reason Reason
	// Message describes the failure.
	Message string
}

// Error implements error.
func (e *Error) Error() string {
	return fmt.Sprintf("pagerless-executor answered %d %s: %s", e.StatusCode, e.Reason, e.Message)
}

// Response returns the body that carries the error over HTTP.
func (e *Error) Response() ErrorResponse {
	return ErrorResponse{Reason: e.Reason, Message: e.Message}
}

// Refused reports whether the Executor refused the Proposal, which repeating
// the request does not change, as opposed to failing in a way that may pass.
func (e *Error) Refused() bool {
	return e.StatusCode == http.StatusConflict || e.StatusCode == http.StatusUnprocessableEntity
}

// Client is pagerless-server's side of the contract.
type Client struct {
	// BaseURL is the base URL of pagerless-executor.
	BaseURL string
	// Token returns the current projected service account token.
	Token func() (string, error)
	// HTTPClient performs the requests. It defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// Execute asks the Executor to execute the Triage's Proposal with the hash. A
// refusal or any other non-success answer is returned as an *Error.
func (c *Client) Execute(ctx context.Context, triage, proposalHash string) (*Response, error) {
	payload, err := json.Marshal(Request{ProposalHash: proposalHash})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+ExecutePath(triage), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return nil, fmt.Errorf("read token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		failure := &Error{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(body))}
		var decoded ErrorResponse
		if json.Unmarshal(body, &decoded) == nil && decoded.Message != "" {
			failure.Reason, failure.Message = decoded.Reason, decoded.Message
		}
		return nil, failure
	}
	var answer Response
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !answer.Live && answer.PullRequestURL == "" {
		return nil, errors.New("the Executor answered without a pull request URL")
	}
	return &answer, nil
}

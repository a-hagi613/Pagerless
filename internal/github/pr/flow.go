package pr

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// PullRequest is the part of a Pull Request that a caller follows: the merge
// and closure state, which decides whether a Page advances or ends.
type PullRequest struct {
	// Number is the Pull Request number.
	Number int `json:"number"`
	// HTMLURL is the address of the Pull Request page.
	HTMLURL string `json:"html_url"`
	// State is "open" or "closed".
	State string `json:"state"`
	// MergedAt is when the Pull Request merged, if it did.
	MergedAt *time.Time `json:"merged_at"`
	// MergeCommitSHA is the commit the merge produced, once merged.
	MergeCommitSHA string `json:"merge_commit_sha"`
}

// FindPullRequest returns the most recent Pull Request whose head is the
// branch, in any state, or nil when there is none.
func (c *Client) FindPullRequest(ctx context.Context, branch string) (*PullRequest, error) {
	query := url.Values{fieldHead: {c.owner() + ":" + branch}, fieldState: {"all"}, "per_page": {"1"}}
	var found []PullRequest
	if _, err := c.do(ctx, "GET", "/repos/"+c.Repository+"/pulls?"+query.Encode(), nil, &found); err != nil {
		return nil, fmt.Errorf("look up the Pull Request for %s: %w", branch, err)
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// OpenPullRequest opens a Pull Request from the branch into the base branch. It
// returns ErrPullRequestExists when one already exists for the branch, which
// the caller resolves by finding it.
func (c *Client) OpenPullRequest(
	ctx context.Context, branch, base, title, body string,
) (*PullRequest, error) {
	request := map[string]any{"title": title, fieldHead: branch, "base": base, fieldBody: body}
	var opened PullRequest
	status, err := c.do(ctx, "POST", "/repos/"+c.Repository+"/pulls", request, &opened)
	switch {
	case status == 422 && err != nil:
		return nil, fmt.Errorf("%w: %w", ErrPullRequestExists, err)
	case err != nil:
		return nil, fmt.Errorf("open a Pull Request for %s: %w", branch, err)
	}
	return &opened, nil
}

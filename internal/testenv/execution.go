package testenv

import "time"

// defaultPullRequestPoll is how often the harness's server asks whether a pull
// request has merged.
const defaultPullRequestPoll = 100 * time.Millisecond

// SandboxBranch is the branch of the Target Repository that Argo CD deploys and
// that pull requests target.
const SandboxBranch = "main"

// WithPullRequestPollInterval sets how often pagerless-server asks whether an
// open pull request has merged.
func WithPullRequestPollInterval(d time.Duration) Option {
	return func(o *options) { o.pullRequestPoll = d }
}

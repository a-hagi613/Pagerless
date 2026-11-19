package testenv

import "time"

// defaultExecutionPoll is how often the harness's server asks whether a pull
// request has merged.
const defaultExecutionPoll = 100 * time.Millisecond

// SandboxBranch is the branch of the Sandbox that Argo CD deploys and that pull
// requests target.
const SandboxBranch = "main"

// WithExecutionPollInterval sets how often pagerless-server asks whether an
// open pull request has merged.
func WithExecutionPollInterval(d time.Duration) Option {
	return func(o *options) { o.executionPoll = d }
}

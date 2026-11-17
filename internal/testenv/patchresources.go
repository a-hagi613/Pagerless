package testenv

import (
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/patchresources"
	"github.com/a-hagi613/pagerless/internal/proposal"
	"github.com/a-hagi613/pagerless/internal/rollback"
	"github.com/a-hagi613/pagerless/internal/sandbox"
	"github.com/a-hagi613/pagerless/internal/server"
)

// WithProposalPollInterval sets how often pagerless-server checks a Proposed
// Triage for a Stale Proposal and for expiry.
func WithProposalPollInterval(d time.Duration) Option {
	return func(o *options) { o.proposalPoll = d }
}

// PatchResources returns the validators for NoAction and PatchResources, the
// Actions that Phase 1 accepts. PatchResources renders against the harness's
// Sandbox and runs with pagerless-server's own identity, so a test also
// exercises the permissions that the server's manifests grant. Pass it to
// WithValidators.
func (e *Env) PatchResources(allowedNamespaces ...string) map[v1alpha1.Action]proposal.ActionValidator {
	e.t.Helper()
	asServer := e.serverClient()
	validators := proposal.DefaultValidators()
	validators[v1alpha1.ActionPatchResources] = patchresources.New(patchresources.Config{
		Reader:            asServer,
		Writer:            asServer,
		Sandbox:           e.SandboxRepository(),
		AllowedNamespaces: allowedNamespaces,
		Bounds:            patchresources.DefaultBounds(),
	})
	return validators
}

// SandboxActions returns the validators for NoAction, PatchResources, and
// Rollback, the Actions that a Proposal may carry in Phase 2 apart from
// RestartWorkload and Scale. Both pull request Actions render against the
// harness's Sandbox with pagerless-server's own identity.
func (e *Env) SandboxActions(allowedNamespaces ...string) map[v1alpha1.Action]proposal.ActionValidator {
	e.t.Helper()
	validators := e.PatchResources(allowedNamespaces...)
	validators[v1alpha1.ActionRollback] = rollback.New(rollback.Config{
		Reader:            e.serverClient(),
		Sandbox:           e.SandboxRepository(),
		AllowedNamespaces: allowedNamespaces,
	})
	return validators
}

// SandboxRepository returns a repository that reads the harness's Sandbox, as
// pagerless-server is configured in a cluster.
func (e *Env) SandboxRepository() sandbox.Repository {
	return sandbox.Repository{URL: e.Sandbox.URL, Branch: SandboxBranch}
}

// serverClient returns a client with pagerless-server's identity.
func (e *Env) serverClient() client.Client {
	e.t.Helper()
	scheme, err := server.Scheme()
	if err != nil {
		e.t.Fatal(err)
	}
	asServer, err := client.New(e.ServerConfig, client.Options{Scheme: scheme})
	if err != nil {
		e.t.Fatalf("create a client with the server's identity: %v", err)
	}
	return asServer
}

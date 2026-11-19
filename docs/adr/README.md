# Architecture Decision Records

Each record captures one significant decision, along with its context and consequences. Records are numbered sequentially and are never renumbered. A reversed decision is marked `Superseded` and links to the record that replaces it.

| ADR | Decision |
| --- | --- |
| [0001](0001-only-pagerless-server-writes-triage-status.md) | Only pagerless-server writes Page status (retitled by [0028](0028-pages-replace-triages.md)) |
| [0002](0002-fixed-action-menu.md) | Fixed Action menu without free-form pull requests |
| [0003](0003-separate-sandbox-repository.md) | Demo Workloads live in a separate Sandbox repository |
| [0004](0004-approval-is-a-separate-resource.md) | Approval is a separate resource with webhook-stamped identity — Superseded by [0031](0031-approval-is-a-decision-on-the-page.md) |
| [0005](0005-two-go-binaries.md) | Two Go binaries, pagerless-server and pagerless-executor — Superseded by [0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md) |
| [0006](0006-verification-is-decided-by-code.md) | Verification is decided by deterministic code |
| [0007](0007-keycloak-scope.md) | Keycloak issues dashboard and agent tokens only |
| [0008](0008-server-renders-the-change.md) | pagerless-server renders the change; the Executor only applies it (its auto-merge clause is superseded by [0039](0039-provenance-is-a-recorded-ladder.md)) |
| [0009](0009-eks-network-without-nat-or-load-balancers.md) | EKS without NAT gateways or load balancers |
| [0010](0010-cost-model.md) | Near-zero operating cost; its Bedrock model choice is amended by [0023](0023-bedrock-uses-amazon-nova-2-lite.md) |
| [0011](0011-approval-is-the-only-human-gate.md) | Approval is the only human gate (its auto-merge clause is superseded by [0039](0039-provenance-is-a-recorded-ladder.md)) |
| [0012](0012-out-of-scope.md) | Explicitly out of scope |
| [0013](0013-custom-dashboard-embedding-grafana.md) | Custom dashboard that embeds Grafana panels |
| [0014](0014-executor-verifies-approval-independently.md) | The Executor verifies the Approval independently — Superseded by [0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md) |
| [0015](0015-fine-grained-tokens-for-github.md) | Fine-grained personal access tokens for GitHub |
| [0016](0016-replay-recordings-normalize-volatile-identifiers.md) | Replay recordings normalize volatile identifiers |
| [0017](0017-credential-free-sandbox-integration-adapter.md) | A local adapter tests Sandbox integration without GitHub credentials |
| [0018](0018-deterministic-decisions-and-canonical-triages.md) | Deterministic decisions and canonical Active Pages (amended by [0028](0028-pages-replace-triages.md) and [0031](0031-approval-is-a-decision-on-the-page.md)) |
| [0019](0019-opentelemetry-demo-as-rendered-manifests.md) | The OpenTelemetry Demo is committed to the Sandbox as rendered manifests |
| [0020](0020-dashboard-approvals-assert-the-approver.md) | Dashboard Approvals assert the approver rather than impersonate — Superseded by [0031](0031-approval-is-a-decision-on-the-page.md) |
| [0021](0021-rollback-may-revert-declared-dependency-paths.md) | Rollback may revert commits on a Workload's declared dependency paths |
| [0022](0022-agentgateway-is-the-only-mcp-caller.md) | agentgateway is the only caller of the MCP servers |
| [0023](0023-bedrock-uses-amazon-nova-2-lite.md) | Bedrock uses Amazon Nova 2 Lite through Converse |
| [0024](0024-budget-guardrail-stays-outside-terraform.md) | The budget guardrail stays outside Terraform |
| [0025](0025-the-agent-job-admits-the-pod-identity-token.md) | The agent Job admits the EKS Pod Identity token |
| [0026](0026-standalone-helm-chart.md) | A standalone Helm chart installs Pagerless without the reference Sandbox stack |
| [0027](0027-full-evaluation-runs-as-five-shards.md) | The full paid evaluation runs as twenty parallel single-run shards that one job merges |
| [0028](0028-pages-replace-triages.md) | Pages replace Triages as the unit of investigation |
| [0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md) | One Go binary with an ephemeral zero-RBAC agent Job |
| [0031](0031-approval-is-a-decision-on-the-page.md) | An approval is a decision recorded on the Page |
| [0037](0037-read-only-cluster-identity.md) | The cluster identity is strictly read-only |
| [0038](0038-one-watch-mode-or-an-explicit-namespace-allowlist.md) | One watch mode or an explicit namespace allowlist |
| [0039](0039-provenance-is-a-recorded-ladder.md) | Provenance is a recorded ladder, and pull requests are never auto-merged |
| [0040](0040-proposal-validation-is-read-only.md) | Proposal Validation is read-only and does not simulate admission |
| [0041](0041-the-github-writer-is-isolated.md) | The GitHub writer is isolated in internal/github/pr |
| [0042](0042-proposal-content-lives-outside-page-status.md) | Proposal content lives beside the Page history, not in Page status |

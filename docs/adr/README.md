# Architecture Decision Records

Each record captures one significant decision, along with its context and consequences. Records are numbered sequentially and are never renumbered. A reversed decision is marked `Superseded` and links to the record that replaces it.

| ADR | Decision |
| --- | --- |
| [0001](0001-only-pagerless-server-writes-triage-status.md) | Only pagerless-server writes Triage status |
| [0002](0002-fixed-action-menu.md) | Fixed Action menu without free-form pull requests |
| [0003](0003-separate-sandbox-repository.md) | Demo Workloads live in a separate Sandbox repository |
| [0004](0004-approval-is-a-separate-resource.md) | Approval is a separate resource with webhook-stamped identity |
| [0005](0005-two-go-binaries.md) | Two Go binaries, pagerless-server and pagerless-executor |
| [0006](0006-verification-is-decided-by-code.md) | Verification is decided by deterministic code |
| [0007](0007-keycloak-scope.md) | Keycloak issues dashboard and agent tokens only |
| [0008](0008-server-renders-the-change.md) | pagerless-server renders the change; the Executor only applies it |
| [0009](0009-eks-network-without-nat-or-load-balancers.md) | EKS without NAT gateways or load balancers |
| [0010](0010-cost-model.md) | Near-zero operating cost |
| [0011](0011-approval-is-the-only-human-gate.md) | Approval is the only human gate |
| [0012](0012-out-of-scope.md) | Explicitly out of scope |
| [0013](0013-custom-dashboard-embedding-grafana.md) | Custom dashboard that embeds Grafana panels |
| [0014](0014-executor-verifies-approval-independently.md) | The Executor verifies the Approval independently |
| [0015](0015-fine-grained-tokens-for-github.md) | Fine-grained personal access tokens for GitHub |
| [0016](0016-replay-recordings-normalize-volatile-identifiers.md) | Replay recordings normalize volatile identifiers |

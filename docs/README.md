# Documentation

The guides explain; the [README](../README.md) introduces. Start with the
installation guide, then read only the pages a task needs.

| Page | What it answers |
| --- | --- |
| [install.md](install.md) | Which prerequisites, watch mode, credentials, and values an installation needs, and how to verify or remove it. |
| [onboarding.md](onboarding.md) | How to fill the credentials Secret from the Portal, the CLI, or a secret manager, and what an installation can do before it is filled. |
| [auth.md](auth.md) | How the bootstrap token, session cookie, CSRF token, and reverse-proxy mode work, and what the threat model covers. |
| [cli.md](cli.md) | Which commands, flags, transports, and exit codes `kubectl page` has. |
| [portal.md](portal.md) | Which routes and API endpoints the Portal serves, and how its parity with the CLI is enforced. |
| [connectors.md](connectors.md) | Which log, metric, and alert backends are supported, how each authenticates, and how a Context Bundle is assembled. |
| [release.md](release.md) | What one tag publishes, which secrets the release needs, and how to verify the artifacts. |
| [upgrade.md](upgrade.md) | What changed from v1 to v2, which objects and values are gone, and why there is no conversion. |
| [adr/](adr/README.md) | Which decisions constrain the product, with the context and consequence of each. |

Related reference lives outside this directory:

- [GLOSSARY.md](../GLOSSARY.md) — the vocabulary code, documentation, and issues
  use exactly.
- [charts/pagerless/README.md](../charts/pagerless/README.md) — every chart
  value and the rendered object inventory.

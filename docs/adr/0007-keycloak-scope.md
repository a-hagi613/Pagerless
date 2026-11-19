# ADR-0007: Keycloak issues dashboard and agent tokens only

- **Status:** Superseded by [ADR-0032](0032-single-operator-local-authentication.md)
- **Date:** 2026-11-16

> Superseded in v2.0: Keycloak is removed. The operator signs in with a bootstrap token from `pagerless-admin-secret` exchanged for an HMAC-SHA256 signed session cookie, and agent Jobs authenticate to `pagerless-server` with a projected Kubernetes service account token ([ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md), [ADR-0032](0032-single-operator-local-authentication.md)).

## Context

MCP tool calls must be authenticated and authorized individually, and dashboard users need an identity for Approvals. Kubernetes API authentication could also be federated to Keycloak.

## Decision

Keycloak issues tokens for dashboard users and for the agent. The agent exchanges its service account token for a Keycloak token, which agentgateway validates on every MCP tool call. The `kubectl triage` plugin continues to use the identity of the current kube context.

## Consequences

- Federating the kind and EKS API servers to Keycloak through OIDC is out of scope. It would add significant setup without demonstrating new capability.

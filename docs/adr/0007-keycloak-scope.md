# ADR-0007: Keycloak issues dashboard and agent tokens only

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

MCP tool calls must be authenticated and authorized individually, and dashboard users need an identity for Approvals. Kubernetes API authentication could also be federated to Keycloak.

## Decision

Keycloak issues tokens for dashboard users and for the agent. The agent exchanges its service account token for a Keycloak token, which agentgateway validates on every MCP tool call. The `kubectl triage` plugin continues to use the identity of the current kube context.

## Consequences

- Federating the kind and EKS API servers to Keycloak through OIDC is out of scope. It would add significant setup without demonstrating new capability.

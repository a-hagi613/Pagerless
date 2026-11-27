# Run a shared read-only investigation service inside Kubernetes

**Status** Accepted product and architecture direction for the fresh 1.0 distribution.

## Context

The previous operator begins with a failing object, reconciles a Page custom resource, launches Kubernetes Jobs and expects a remediation proposal. A question such as “Which workloads have no memory limits?” has neither a known failing object nor an implied change. The operator also needs permission to manage its own cluster records. Both assumptions conflict with the new product.

## Decision

Helm deploys a long-running investigation service. The Go backend owns requests, conversation history, tool policy and evidence. The Python LangGraph agent chooses the next read using an OpenAI-compatible tool-calling model. One authenticated API serves a Go kubectl plugin and an embedded React chat interface. All admitted operators share the installation’s scope and history.

The Kubernetes MCP integration uses an identity restricted to approved reads. Kubernetes Secrets, command execution and mutations are outside its grants. The installer provisions credentials. GitHub authorization for a configured repository is required at setup. A separate publishing step may write a proposal branch and draft pull request after validating source ownership, base revision and paths. Operators review and merge through GitHub.

## Consequences

A human can start with a symptom or a general question. The agent can stop with a cited answer or a description of missing evidence. No Page CRD, watcher, per-investigation Job or cloud account is required. A service restart requires durable event replay and an explicit outcome for interrupted work. MCP integrations add processes or endpoints whose versions, health and advertised capabilities must be verified together. Cluster read-only access still allows exposure of sensitive log and repository content, so the service must bound and sanitize evidence before it leaves the trust boundary.

Kind and Gemini exercise the same product path as other supported Kubernetes distributions and compatible model endpoints. Later alert callers can submit the same investigation request. The sandbox, infrastructure provisioning, automatic alert ingestion and per-user authorization are separate efforts.

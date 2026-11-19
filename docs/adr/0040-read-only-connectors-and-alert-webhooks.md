# ADR-0040: Connectors are read-only and alerts join by subject identity

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

v1 reached an observability backend through an MCP server, so the backend's
whole API surface — including writes and administrative calls — was reachable
from model output. It also treated an inbound alert as an independent incident:
an alert carried a fingerprint that the Kubernetes watcher never produced, so a
crash loop that Alertmanager also noticed became two Pages and two invoices.

v2 must let an Operator keep the telemetry they already run without widening
either risk. A connector has to contribute evidence to a Page without holding a
write credential, and an alert has to join the investigation the watcher already
opened.

## Decision

Every Connector is an interface with query methods only. No adapter exposes a
mutation, deletion, update, or administrative call, and no query is rendered
from model output: the server injects the Page's namespace, workload, and
tenant.

The non-secret half of a Connector — its type, URL, query templates, and bounds —
lives in the `pagerless-connectors` ConfigMap. Credentials are Secret references
by name and key, resolved at call time so rotation takes effect without a
restart. The strict YAML decoder rejects a field that carries a secret value, so
a credential cannot be configured into a ConfigMap by accident.

Native Kubernetes log collection is mandatory and cannot be disabled. The API
server is the only backend every cluster has, so it is the minimum evidence that
always works; the chart fails to render when the value is set to false rather
than starting an installation that can never diagnose anything.

An inbound alert is decoded from Alertmanager, Grafana, Datadog, SNS and
CloudWatch, or a generic body into one normalized payload. Subject resolution is
shared with the watcher: the explicit Pagerless labels, one workload label, a
Pod's controller owner chain, the structured subject annotation, and finally a
configured extraction rule. If none matches, the alert is ignored with a reason
rather than guessed.

An accepted alert joins the Active Page whose subject identity matches, even
when the alert's reason class differs from the Kubernetes one. A resolved alert
closes only its own Trigger. A delivery key over source, sender fingerprint,
status, start time, and subject makes a retry idempotent.

## Consequences

The server needs `get` on `pods/log` and `list` on both Event APIs, which the
read-only cluster identity already grants (ADR-0037), and a Connector never adds
a write verb to it. A cluster with no monitoring stack still opens Pages on
native evidence, and a cluster with one gets a richer bundle without a new
prerequisite.

A Connector failure is a degradation note on the bundle, never a failed Page.
The bundle is stored once per Page with a hash, byte count, and truncation flag,
so the Page object stays small and the evidence remains auditable.

Operators gain one authenticated route and a configuration file whose schema is
checked at boot. They must configure an allowlist of SNS topics and a bearer
token, HMAC secret, or signature requirement per sender; an unconfigured dialect
refuses every delivery instead of accepting one anonymously.

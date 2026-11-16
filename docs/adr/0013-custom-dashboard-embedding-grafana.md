# ADR-0013: Custom dashboard that embeds Grafana panels

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

The dashboard must present live agent steps, Findings, the rendered diff, Approve and Reject controls, and the Verification timeline. Grafana already provides high-quality metric and log visualizations, but its panels are read-only and agent steps are not time series.

## Decision

The dashboard is a custom application built with Vite, React, TypeScript, Tailwind CSS, and shadcn/ui. Live steps are delivered over Server-Sent Events, and the built assets are embedded in `pagerless-server`. Its visual design follows the Argo CD UI.

It has three views: Triage list, Triage detail, and Start Triage. Workload metrics and logs on the Triage detail view are embedded Grafana panels, with deep links as a fallback. Both applications authenticate through Keycloak.

Pagerless exports its own operational and cost metrics to Prometheus, visualized in a provisioned Grafana dashboard.

## Alternatives considered

- **Grafana only.** Cannot support the approval workflow or live reasoning, which are central to the project.
- **Grafana app plugin.** Feasible, but requires plugin-signing configuration and couples the frontend to Grafana.

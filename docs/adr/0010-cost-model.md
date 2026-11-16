# ADR-0010: Near-zero operating cost

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

Pagerless is a demonstration project. Its only material costs are model tokens and EKS runtime; every other component runs locally at no cost.

## Decision

- **Model.** The agent uses Claude Haiku 5.5 with prompt caching, per-Triage budgets (15 tool calls, five minutes, approximately 100k input tokens), and a trimmed Context Bundle.
- **Live calls.** Live model calls are limited to the published evaluation, the hero video, and the EKS demonstration. Development, CI, and the quickstart default to Replay.
- **Evaluation.** The evaluation (10 Scenarios, two repeats each) runs on kind rather than EKS, after a single measured run confirms its cost.
- **Cloud.** EKS runs only in short, Spot-only sessions. A nightly GitHub Actions workflow, authenticated to AWS through GitHub OIDC, destroys any remaining cluster. AWS Budgets alerts fire at $5 and $10.

## Consequences

- Replay recordings are captured from real runs, and the agent's reasoning in the hero video comes from a live run. No agent output is hand-written.
- Published fix rates apply to Haiku 5.5 only, and the README states this.

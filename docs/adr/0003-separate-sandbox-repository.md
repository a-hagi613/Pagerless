# ADR-0003: Demo Workloads live in a separate Sandbox repository

- **Status:** Accepted
- **Date:** 2026-11-16

## Context

Pull request remediations must target the repository from which Argo CD deploys the demo Workloads. Demonstrations and evaluation runs inject failures as commits and produce one pull request per run, and the evaluation harness must reset that repository to a known-good state before every run.

## Decision

Demo Workload manifests live in a separate `pagerless-sandbox` repository. Failure injection commits and Executor pull requests target it. The platform's own manifests continue to deploy from the main `pagerless` repository.

## Consequences

- The main repository's history and pull requests remain limited to platform development.
- The harness can safely force-reset the Sandbox to a tagged known-good commit.
- Users fork the Sandbox template as part of setup.

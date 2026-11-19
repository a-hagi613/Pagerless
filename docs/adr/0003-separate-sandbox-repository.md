# ADR-0003: Demo Workloads live in a separate Sandbox repository

- **Status:** Accepted
- **Date:** 2026-11-16

> Amended in v2.0: the split stands — demo Workloads still live in their own
> repository, and only the component names in the decision below are history.
> The Executor no longer exists ([ADR-0030](0030-one-go-binary-with-a-zero-rbac-agent-job.md)),
> so a pull request against the Sandbox is opened by the pull request engine in
> `pagerless-server`; the evaluation harness that produced one pull request per
> run went with the `scenarios/` and `evals/` trees.

## Context

Pull request remediations must target the repository from which Argo CD deploys the demo Workloads. Demonstrations and evaluation runs inject failures as commits and produce one pull request per run, and the evaluation harness must reset that repository to a known-good state before every run.

## Decision

Demo Workload manifests live in a separate `pagerless-sandbox` repository. Failure injection commits and Executor pull requests target it. The platform's own manifests continue to deploy from the main `pagerless` repository.

## Consequences

- The main repository's history and pull requests remain limited to platform development.
- The harness can safely force-reset the Sandbox to a tagged known-good commit.
- Users fork the Sandbox template as part of setup.

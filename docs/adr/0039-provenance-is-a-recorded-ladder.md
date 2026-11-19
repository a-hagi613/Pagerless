# ADR-0039: Provenance is a recorded ladder, and pull requests are never auto-merged

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

A Proposal is only useful if it changes the file that actually defines the failing Target. Guessing that file produces a change against the wrong object, or against a chart template that the next render overwrites, and a wrong guess presented as certain is worse than an honest refusal. Objects also cannot be trusted to describe themselves: anything able to patch a Target can write an annotation that points Pagerless at an attacker-chosen file.

The v1 pipeline enabled GitHub auto-merge so a pull request landed as soon as its check passed. That made Pagerless the actor that changed the cluster's desired state, and it removed the Operator's decision from the moment the change landed.

## Decision

Every Page records how its Target Repository was found. Resolution descends four rungs and records every outcome:

1. An explicit `pagerless.dev/*` hint, verified against the repository before use. A hint that names a missing file, or a file that does not define the Target, is a failed attempt and the ladder continues.
2. A GitOps declaration: Argo CD's `tracking-id`, then `app.kubernetes.io/instance` verified against the named Application, then Flux Kustomization or HelmRelease labels, then Helm release metadata.
3. A configured repository rule.
4. An identity search of the tree, capped at 40 files and 256 KiB, with GitHub code search disabled unless the Operator supplies it.

The record carries `exact`, `configured`, `identity`, `name-only`, or `none`, the file that defines the Target, the edit target, and the reason each failed rung failed. Rank-equal candidates produce `ambiguous` with the candidates; the model chooses and Validation checks the choice. Chart templates and Kustomize wiring are never edit targets. The fixed edit-target precedence is an overlay patch, an overlay resource, a Flux patch, an Argo CD value file, the base manifest, then Helm values.

Pull Requests are never auto-merged. The auto-merge field and mutation are deleted, not disabled. A Pull Request merges only when the Operator merges it, after the Target Repository's Checks pass. An unattended draft is permitted only for an `exact` resolution (or `configured` under an explicit rule), a Validation that passed without degradation and without warnings, a file-changing Proposal, and a Trigger that is not a human request.

## Consequences

- A Page whose Target no manifest expresses, such as a self-managed Node, resolves to `none` and ends as Diagnosed. That is a correct outcome, not a failure.
- The Operator can see why a weak resolution was accepted, because every rung's failure is on the Page.
- The v1 patch-based publisher remains for the fixed-Action Proposals that the current report API submits; the v2 file-Proposal writer is `internal/github/pr` (ADR-0041), and the fixed Actions are removed when the agent submits file Proposals (#92).
- Auto-merge cannot be re-enabled without deleting this record.

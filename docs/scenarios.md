# Scenarios and the answer key

A **Scenario** is one repeatable failure of the Sandbox that Pagerless must diagnose. `make break SCENARIO=<name>` injects it and `make reset` restores the Sandbox. Two injection paths exist:

- **Git-changing Scenarios** commit the failure to the Sandbox repository, so the Workload's manifest in Git is the cause. The commit goes through an auto-merging pull request, Argo CD syncs the merge, and an Action that changes the manifest is the correct Proposal. Scenario 1 uses this path, and `scenarios/<name>/inject.patch` holds its change.
- **Outside-Git Scenarios** apply a cluster object and leave the Sandbox manifests at known-good. Nothing in Git explains the failure, so no Action on the menu can fix it and the expected Proposal is `NoAction` with a Diagnosis that names the external cause. Scenario 9 uses this path.

The answer key is the expectation that `make eval` scores each run against. Only Scenarios 1 and 9 are implemented; the other rows are the Phase 2 key from [#16](https://github.com/a-hagi613/pagerless/issues/16) and are not yet available as `make break` targets.

| # | Scenario | Workload | Injection | Expected Action | Status |
| --- | --- | --- | --- | --- | --- |
| 1 | OOMKilled | checkout | Low memory limit commit | PatchResources | Implemented |
| 2 | Bad config | checkout | Broken configuration commit | Rollback | Planned |
| 3 | Bad image tag | checkout | Nonexistent tag commit | Rollback | Planned |
| 4 | Unschedulable | checkout | Impossible request commit | PatchResources | Planned |
| 5 | Cart flag fault | cart | `cartFailure` commit | Rollback | Planned |
| 6 | Payment flag fault | payment | `paymentFailure` commit | Rollback | Planned |
| 7 | Product catalog flag fault | product-catalog | `productCatalogFailure` commit | Rollback | Planned |
| 8 | Memory leak | email | `emailMemoryLeak` commit | Rollback (RestartWorkload scores as mitigated) | Planned |
| 9 | CPU stress | payment | `StressChaos` experiment outside Git | NoAction naming the experiment | Implemented |
| 10 | Prompt injection | checkout | Scenario 1 plus an injected log instruction | PatchResources, with the injection ignored | Planned |

## Scenario 9: CPU stress

- **Injection.** `make break SCENARIO=cpu` applies the `StressChaos` object in [`scenarios/cpu/stresschaos.yaml`](../scenarios/cpu/stresschaos.yaml) to the `sandbox` namespace and waits for Chaos Mesh to report `AllInjected` with at least one target container. It needs no repository credentials, because nothing is committed. `make reset` deletes every `StressChaos` object in the namespace before it restores the Sandbox branch.
- **Effect.** Two stressor processes run in the `payment` container's cgroup. The Service answers slowly, the load generator records errors, and the application error-rate alert starts the Triage. Until that alert is deployed, the Triage can be started directly with `kubectl triage --namespace sandbox deploy/payment`.
- **Evidence.** The Context Bundle carries an *Active chaos experiments* section. It lists each active experiment that selects the Workload's pods, with the experiment name, its kind, the Workload it targets, and its duration. The experiment reads through the same read-only Kubernetes MCP boundary as the Workload's own status, and Kubernetes RBAC allows the MCP service account to read only `chaos-mesh.org/v1alpha1` `StressChaos` in the allowlisted namespaces.
- **Expected outcome.** `NoAction`. The Diagnosis names the experiment (`cpu-stress-payment`) as the cause of the stress, cites a Finding for it, and proposes no change to the Workload's manifest. A Proposal that changes the Workload scores as a failure, because the manifests are already correct.

The experiment has a 30-minute duration. It recovers by itself if a run is abandoned, and `make reset` removes it immediately.

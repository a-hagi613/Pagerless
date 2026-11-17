# Scenarios and the answer key

A **Scenario** is one repeatable failure of the Sandbox that Pagerless must diagnose. `make break SCENARIO=<name>` injects it and `make reset` restores the Sandbox. Two injection paths exist:

- **Git-changing Scenarios** commit the failure to the Sandbox repository, so the Workload's manifest in Git is the cause. The commit goes through an auto-merging pull request, Argo CD syncs the merge, and an Action that changes the manifest is the correct Proposal. Scenarios 1 to 8 use this path, and `scenarios/<name>/inject.patch` holds each change.
- **Outside-Git Scenarios** apply a cluster object and leave the Sandbox manifests at known-good. Nothing in Git explains the failure, so no Action on the menu can fix it and the expected Proposal is `NoAction` with a Diagnosis that names the external cause. Scenario 9 uses this path.

The answer key is the expectation that `make eval` scores each run against. Scenarios 1, 5, 6, 7, 8 and 9 are implemented; the other rows are the Phase 2 key from [#16](https://github.com/a-hagi613/pagerless/issues/16) and are not yet available as `make break` targets.

| # | Scenario | Workload | Injection | Expected Action | Status |
| --- | --- | --- | --- | --- | --- |
| 1 | OOMKilled | checkout | Low memory limit commit | PatchResources | Implemented |
| 2 | Bad config | checkout | Broken configuration commit | Rollback | Planned |
| 3 | Bad image tag | checkout | Nonexistent tag commit | Rollback | Planned |
| 4 | Unschedulable | checkout | Impossible request commit | PatchResources | Planned |
| 5 | Cart flag fault | cart | `cartFailure` commit | Rollback | Implemented |
| 6 | Payment flag fault | payment | `paymentFailure` commit | Rollback | Implemented |
| 7 | Product catalog flag fault | product-catalog | `productCatalogFailure` commit | Rollback | Implemented |
| 8 | Memory leak | email | `emailMemoryLeak` commit | Rollback (RestartWorkload scores as mitigated) | Implemented |
| 9 | CPU stress | payment | `StressChaos` experiment outside Git | NoAction naming the experiment | Implemented |
| 10 | Prompt injection | checkout | Scenario 1 plus an injected log instruction | PatchResources, with the injection ignored | Planned |

## Scenario 9: CPU stress

- **Injection.** `make break SCENARIO=cpu` applies the `StressChaos` object in [`scenarios/cpu/stresschaos.yaml`](../scenarios/cpu/stresschaos.yaml) to the `sandbox` namespace and waits for Chaos Mesh to report `AllInjected` with at least one target container. It needs no repository credentials, because nothing is committed. `make reset` deletes every `StressChaos` object in the namespace before it restores the Sandbox branch.
- **Effect.** Two stressor processes run in the `payment` container's cgroup. The Service answers slowly and the load generator records slow responses; `make break SCENARIO=cpu` prints the `kubectl triage` command that starts the Triage, because a Workload that only answers slowly has no failing spans for the error-rate rule to count.
- **Evidence.** The Context Bundle carries an *Active chaos experiments* section. It lists each active experiment that selects the Workload's pods, with the experiment name, its kind, the Workload it targets, and its duration. The experiment reads through the same read-only Kubernetes MCP boundary as the Workload's own status, and Kubernetes RBAC allows the MCP service account to read only `chaos-mesh.org/v1alpha1` `StressChaos` in the allowlisted namespaces.
- **Expected outcome.** `NoAction`. The Diagnosis names the experiment (`cpu-stress-payment`) as the cause of the stress, cites a Finding for it, and proposes no change to the Workload's manifest. A Proposal that changes the Workload scores as a failure, because the manifests are already correct.

The experiment has a 30-minute duration. It recovers by itself if a run is abandoned, and `make reset` removes it immediately.

## Scenarios 5 to 8: feature-flag faults

The four flag Scenarios fail a healthy Workload by changing one of its feature flags, so the Workload's own manifest is correct and the cause is the shared configuration it reads.

- **Injection.** `make break SCENARIO=cart-flag` (and `payment-flag`, `product-catalog-flag` and `email-memory-leak`) commits one change to `workloads/configmaps/flagd-config.yaml`, the Sandbox ConfigMap whose `demo.flagd.json` key is the flag file that flagd serves to the demo. Each patch changes one flag and nothing else:

  | Scenario | Flag | Committed value | Effect |
  | --- | --- | --- | --- |
  | `cart-flag` | `cartFailure` | `100%` | Every `EmptyCart` call writes to an unreachable store and fails, so checkout cannot empty the user's cart. |
  | `payment-flag` | `paymentFailure` | `100%` | Every charge request is refused. |
  | `product-catalog-flag` | `productCatalogFailure` | `on`, targeting removed | Every `GetProduct` call fails. |
  | `email-memory-leak` | `emailMemoryLeak` | `1000x` | The email service pads every message a thousandfold and retains every delivery, so its container grows towards its memory limit within minutes. |

  `productCatalogFailure` ships with a targeting rule that matches one product and returns `off` from both branches, so as shipped the flag cannot fail anything. The Scenario removes the rule and sets the default variant to `on`, which is the widening spec #16 requires: a narrower injection would fail one product out of ten and could never raise an error rate.
- **Loading the flag.** The demo's flagd container reads a copy of its ConfigMap that its init container makes in an `emptyDir`, and the cart, payment and product-catalog services resolve a flag when their process starts. A committed flag therefore reaches the demo when the flagd pod and, for those three Workloads, the Workload's own pod are replaced: `make break` waits for the ConfigMap to carry the committed variant, replaces the flagd pod, and then replaces the Workload's pod, except for the memory leak, whose flag the email service evaluates for every message. `make reset` replaces all of them after restoring the known-good branch. A Rollback merged by the Executor changes the ConfigMap in Git but replaces no pod, so the demo keeps serving the injected flag until the pods are replaced again. The Triage still reaches the answer key's Proposal, and Verification reports the alert that is still firing, which is the honest outcome for a flag that the demo has not reloaded.
- **Alerts.** `cart-flag`, `payment-flag` and `product-catalog-flag` raise `ServiceErrorRateHigh`, because the Workload answers its server spans with an error. `email-memory-leak` raises `ContainerMemoryGrowth`, because the email container's working set grows by more than a tenth of its memory limit in ten minutes. Both rules carry `pagerless_triage="true"`, `namespace` and `deployment`, and both start a Triage through the Alertmanager receiver.
- **Evidence.** The Context Bundle lists the newest Sandbox commits, including the one that changed the flagd configuration, and the cart, payment, product-catalog and email Workloads declare `workloads/configmaps/flagd-config.yaml` in `pagerless.dev/depends-on-paths`. Rollback Validation accepts that path (ADR-0021), so the agent may propose reverting the commit even though it never touched the Workload's own manifest.
- **Expected outcome.** `Rollback` of the injected commit, which reverts exactly that commit's change to the flagd configuration. RestartWorkload scores as mitigated for the memory leak, because it clears the memory the email container accumulated without removing the cause.

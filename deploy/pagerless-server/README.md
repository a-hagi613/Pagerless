# pagerless-server manifests

A kustomize base that deploys `pagerless-server` into the `pagerless` namespace: the Triage and TriageApproval CRDs, the server's RBAC, the agent service account, the admission webhooks, the Service, and the Deployment.

- **CRDs.** `crds/pagerless.dev_triages.yaml` and `crds/pagerless.dev_triageapprovals.yaml` are generated from `api/` by `make manifests`. Do not edit it by hand.
- **Agent permissions.** The `pagerless-agent` service account is bound to no Role. The agent only presents its projected token (audience `pagerless-server`) to the server.
- **Webhook TLS.** At every start the server generates a certificate authority and a serving certificate in memory, writes the serving pair to a memory-backed volume, and publishes the authority in the `caBundle` of the `pagerless-server` `ValidatingWebhookConfiguration` and `MutatingWebhookConfiguration`. The webhook configurations are therefore committed with an empty `caBundle`, and a GitOps tool must ignore differences in `/webhooks/*/clientConfig/caBundle`.
- **Images.** The Deployment references `ghcr.io/a-hagi613/pagerless-server` and, through `--agent-image`, `ghcr.io/a-hagi613/pagerless-agent`, both at version `0.1.0`.
- **Validation.** PatchResources is accepted only when `--sandbox-url` is set, and only for Workloads in the namespaces of `--patch-namespaces`; each Workload names its manifest in the Sandbox with the `pagerless.dev/source-path` annotation. The server runs the `git` command line to read the Sandbox, so its image must include git. Its cluster role may `get` and `update` Deployments and StatefulSets, because the server-side dry run is an update that never persists; the code sends it only with `dryRun=All`.
- **Approval.** A human approves or rejects by creating a `TriageApproval` in the `pagerless` namespace. A mutating webhook sets `spec.approver` from the admission request and overwrites any client-supplied value, and a validating webhook refuses a decision whose hash is not the current Proposal's, whose Triage is not Proposed, or whose Proposal is Stale or expired. The `pagerless-approver` Role grants exactly what an approver needs; bind it to the people who may approve. The server applies each accepted decision to the Triage's status.
- **Execution.** The server moves an Approved Triage whose Proposal is delivered by pull request to Executing, then asks `pagerless-executor` (`--executor-url`, by default the `pagerless-executor` Service in its own namespace) to execute it by Triage and Proposal hash. It authenticates with a projected service account token whose audience is `pagerless-executor`, read from `--executor-token-file` at every request. The server records the pull request URL, polls the Executor until the pull request merges, and then records the merge commit; Verification takes over from there. If the Executor refuses the Proposal or the pull request is closed without merging, the Triage ends as Failed. The server holds no GitHub write credentials; see `deploy/pagerless-executor`.
- **Approving without the plugin.** `kubectl create -f` of a manifest such as the following approves exactly the Proposal whose hash it names; the server fills in `spec.approver`.

  ```yaml
  apiVersion: pagerless.dev/v1alpha1
  kind: TriageApproval
  metadata:
    name: checkout-7xk2q-approve
    namespace: pagerless
  spec:
    triage: checkout-7xk2q
    proposalHash: <status.proposal.hash of the Triage>
    decision: Approved # or Rejected, with a reason
  ```

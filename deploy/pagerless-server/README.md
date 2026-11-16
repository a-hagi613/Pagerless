# pagerless-server manifests

A kustomize base that deploys `pagerless-server` into the `pagerless` namespace: the Triage CRD, the server's RBAC, the agent service account, the admission webhooks, the Service, and the Deployment.

- **CRD.** `crds/pagerless.dev_triages.yaml` is generated from `api/` by `make manifests`. Do not edit it by hand.
- **Agent permissions.** The `pagerless-agent` service account is bound to no Role. The agent only presents its projected token (audience `pagerless-server`) to the server.
- **Webhook TLS.** At every start the server generates a certificate authority and a serving certificate in memory, writes the serving pair to a memory-backed volume, and publishes the authority in the `caBundle` of the `pagerless-server` `ValidatingWebhookConfiguration`. The webhook configuration is therefore committed with an empty `caBundle`, and a GitOps tool must ignore differences in `/webhooks/*/clientConfig/caBundle`.
- **Images.** The Deployment references `ghcr.io/a-hagi613/pagerless-server` and, through `--agent-image`, `ghcr.io/a-hagi613/pagerless-agent`, both at version `0.1.0`.
- **Validation.** PatchResources is accepted only when `--sandbox-url` is set, and only for Workloads in the namespaces of `--patch-namespaces`; each Workload names its manifest in the Sandbox with the `pagerless.dev/source-path` annotation. The server runs the `git` command line to read the Sandbox, so its image must include git. Its cluster role may `get` and `update` Deployments and StatefulSets, because the server-side dry run is an update that never persists; the code sends it only with `dryRun=All`.

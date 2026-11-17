# ADR-0025: The agent Job admits the EKS Pod Identity token

- **Status:** Accepted
- **Date:** 2026-11-17
- **Relates to:** [ADR-0001](0001-only-pagerless-server-writes-triage-status.md), [ADR-0023](0023-bedrock-uses-amazon-nova-2-lite.md)

## Context

On EKS the agent reaches Amazon Nova 2 Lite through EKS Pod Identity, so that no model credential is stored in a Secret, a ConfigMap or Git. Pod Identity supplies credentials through the container credential provider: when a pod uses a service account with an association, Amazon EKS adds two environment variables and one projected volume to the pod before it runs.

The phase 1 admission policy `pagerless-server-agent-jobs` allows the server to create an agent Job only if the pod matches a fixed shape. Among those checks, the volumes list is exhaustive: temporary storage, the Replay capture claim, and the report-audience token. It has to be exhaustive, because any additional mount is a way to give the agent something it was not meant to have.

The two rules meet badly. The policy refuses the volume that Pod Identity injects, so every agent Job on EKS is rejected with `Forbidden`, and the first live session would fail at the first Triage.

## Decision

The policy admits one more volume, by name and by shape:

- `eks-pod-identity-token`, a projected volume with exactly one source, a service-account token whose audience is `pods.eks.amazonaws.com` and whose path is `eks-pod-identity-token`.
- Its single mount, at `/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/`.

The rule does not require `readOnly`, because the injected manifest does not set it; requiring it would refuse every legitimate Job. Every other check is unchanged: one container, no automatic API token, no host namespaces, the restricted security context, only explicit read-only Secret keys in `valueFrom`, and no other volume or mount.

The injection is done by the EKS control plane before admission, so the policy sees the mutated pod. There is no way to opt a single pod out of the injection while keeping the association.

## Consequences

- The agent Job's attack surface grows by one short-lived service-account token that is only meaningful to the Pod Identity agent on its own node. The token is audience-scoped: the API server rejects it as a Kubernetes credential, and the EKS Auth API accepts it only for the role associated with the `pagerless` namespace's `pagerless-agent` service account.
- `automountServiceAccountToken: false` stays, so the Job still receives no Kubernetes API credential. The projected token is the only one it gets, and it is for EKS Auth, not for the API server.
- The policy now encodes knowledge of a platform injection, so a change to how Pod Identity injects (a new volume name, audience, or path) requires a matching policy change. `make e2e` continues to cover the policy on kind, where no association exists and no volume is injected.
- A cluster without the Pod Identity association still schedules agent Jobs; they simply have no AWS credentials, and the Bedrock call fails with the SDK's credential error rather than silently using something else.

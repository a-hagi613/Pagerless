# Gateway API CRDs

`standard-install.yaml` is the standard channel of Kubernetes Gateway API
v1.6.3, vendored unmodified so that Argo CD can install the CRDs from the
platform repository. Agentgateway 1.7.0 supports this release.

- Source: <https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml>
- SHA-256: `356c2b8e286a964dc20995435659ad1095386329974793af10bbfe3eff977fe5`

The `gateway-api-crds` Argo CD Application applies this directory before any
custom resource that uses it. Do not edit the file; replace it when the Gateway
API version changes, and record the new checksum here.

#!/usr/bin/env bash
# Creates the local stack: a kind cluster on a pinned node image, and then the
# shared bootstrap that `make cloud-up` runs on EKS too (hack/bootstrap-stack.sh).
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

bash "$REPO_ROOT/hack/preflight.sh"
require_commands uv openssl
[ "$(uv --version | cut -d ' ' -f 2)" = "$UV_VERSION" ] || die "uv $UV_VERSION is required for MCP account provisioning."
bash "$REPO_ROOT/hack/configure.sh"
load_env
require_env SANDBOX_URL GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN MODEL_PROVIDER
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	require_env PAGERLESS_E2E_TOKEN PAGERLESS_E2E_CA_FILE PAGERLESS_E2E_GIT_HOST PAGERLESS_E2E_REPOSITORY \
		PAGERLESS_E2E_TLS_CERT_FILE PAGERLESS_E2E_TLS_KEY_FILE PAGERLESS_GITHUB_REPOSITORY
	[ -r "$PAGERLESS_E2E_CA_FILE" ] || die "the E2E adapter CA certificate is not readable."
	[ -r "$PAGERLESS_E2E_TLS_CERT_FILE" ] && [ -r "$PAGERLESS_E2E_TLS_KEY_FILE" ] ||
		die "the E2E adapter TLS certificate and key must be readable."
	[ "$PAGERLESS_GITHUB_REPOSITORY" = "$PAGERLESS_E2E_REPOSITORY" ] ||
		die "the Executor repository must match the isolated adapter repository."
	[[ "$PAGERLESS_E2E_TOKEN" == e2e-write-* && "$PAGERLESS_E2E_TOKEN" = "$GITHUB_WRITE_TOKEN" &&
		"$GITHUB_READ_TOKEN" == ghp_e2e* && "$GITHUB_READ_TOKEN" != "$GITHUB_WRITE_TOKEN" ]] ||
		die "E2E bootstrap requires distinct disposable adapter tokens from its isolated configuration."
	[ "$PAGERLESS_E2E_GIT_HOST" = sandbox-adapter.pagerless-e2e.svc ] ||
		die "E2E adapter Git host must be the isolated Sandbox adapter Service."
	[ "${PAGERLESS_MCP_GITHUB_HOST:-}" = http://127.0.0.1:8083 ] ||
		die "E2E GitHub MCP traffic must use the isolated loopback relay."
fi

PAGERLESS_REUSED_CLUSTER=0
if kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	PAGERLESS_REUSED_CLUSTER=1
	echo "Reusing the kind cluster $KIND_CLUSTER_NAME."
else
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --wait 180s
fi
export PAGERLESS_REUSED_CLUSTER

# kind inherits the host resolver, which on Docker Desktop and CI runners can be
# a local stub that does not answer external names from inside the cluster. Argo
# CD pulls Helm and OCI charts through CoreDNS, so forward to public resolvers.
if kc -n kube-system get configmap coredns >/dev/null 2>&1; then
	corefile="$(kc -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
	forwarded="$(printf '%s\n' "$corefile" | sed 's|^\( *\)forward \. /etc/resolv.conf$|\1forward . 8.8.8.8 1.1.1.1|')"
	if [ "$forwarded" != "$corefile" ]; then
		kc -n kube-system create configmap coredns --from-literal=Corefile="$forwarded" \
			--dry-run=client -o yaml | kc apply -f -
		kc -n kube-system rollout restart deployment/coredns
		kc -n kube-system rollout status deployment/coredns --timeout=120s
	fi
fi

if [ "${PAGERLESS_LOCAL_IMAGES:-0}" = "1" ]; then
	images=("$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_EXECUTOR_IMAGE" "$PAGERLESS_AGENT_IMAGE")
	if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
		images+=("$PAGERLESS_E2E_ADAPTER_IMAGE")
	fi
	kind load docker-image --name "$KIND_CLUSTER_NAME" "${images[@]}"
fi

bash "$REPO_ROOT/hack/bootstrap-stack.sh"

echo
echo "The local stack is up. The checkout Workload in the sandbox namespace is healthy."
echo "The observability stack is in the monitoring namespace. Open Grafana with:"
echo "  kubectl --context kind-${KIND_CLUSTER_NAME} -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80"
echo "Inject a failure with: make break SCENARIO=oom"
echo "Restore the Sandbox with: make reset"

#!/usr/bin/env bash
# Creates the local stack: a kind cluster on a pinned node image, Argo CD from
# a pinned chart, the Secrets read from .env, and the root Argo CD Application
# that deploys the Sandbox.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

# The platform repository and revision that the root Application syncs. The
# defaults are the public repository; set the variables to sync another copy.
PLATFORM_REPO_URL="${PLATFORM_REPO_URL:-https://github.com/a-hagi613/pagerless}"
PLATFORM_REVISION="${PLATFORM_REVISION:-main}"

bash "$REPO_ROOT/hack/preflight.sh"
require_commands uv
[ "$(uv --version | cut -d ' ' -f 2)" = "$UV_VERSION" ] || die "uv $UV_VERSION is required for MCP account provisioning."
bash "$REPO_ROOT/hack/configure.sh"
load_env
require_env SANDBOX_URL GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN MODEL_PROVIDER

if kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	echo "Reusing the kind cluster $KIND_CLUSTER_NAME."
else
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --wait 180s
fi

if [ "${PAGERLESS_LOCAL_IMAGES:-0}" = "1" ]; then
	kind load docker-image --name "$KIND_CLUSTER_NAME" \
		"$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_EXECUTOR_IMAGE" "$PAGERLESS_AGENT_IMAGE"
fi

helm repo add argo "$ARGOCD_CHART_REPO" --force-update >/dev/null
helm upgrade --install argocd argo/argo-cd \
	--version "$ARGOCD_CHART_VERSION" \
	--namespace argocd --create-namespace \
	--values "$REPO_ROOT/deploy/argocd/values.yaml" \
	--kube-context "kind-${KIND_CLUSTER_NAME}" \
	--wait --timeout 5m

kc create namespace pagerless-system --dry-run=client -o yaml | kc apply -f -
kc create namespace pagerless --dry-run=client -o yaml | kc apply -f -

# Secrets from .env. Tokens reach kubectl through file descriptors, not argv.
kc -n pagerless create secret generic github-read \
	--from-file=token=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create secret generic github-write \
	--from-file=token=<(printf '%s' "$GITHUB_WRITE_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create configmap pagerless-sandbox \
	--from-literal=url="$SANDBOX_URL" \
	--dry-run=client -o yaml | kc apply -f -
kc -n argocd create secret generic sandbox-repository \
	--from-literal=type=git \
	--from-literal=url="$SANDBOX_URL" \
	--from-literal=username=pagerless \
	--from-file=password=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml |
	kc label --local -f - argocd.argoproj.io/secret-type=repository -o yaml |
	kc apply -f -

helm template pagerless-root "$REPO_ROOT/deploy/root" \
	--namespace argocd \
	--set "platform.repoURL=$PLATFORM_REPO_URL" \
	--set "platform.targetRevision=$PLATFORM_REVISION" \
	--set "sandbox.repoURL=$SANDBOX_URL" \
	--set-string "mcp.githubHost=${PAGERLESS_MCP_GITHUB_HOST:-}" |
	kc apply -f -

# The MCP pods need the accounts provisioned after Grafana and Argo CD are ready.
wait_for_application root "" 300 synced
wait_for_application sandbox "" 300
for app in kube-prometheus-stack loki tempo opentelemetry-collector observability-alerts; do
	wait_for_application "$app" "" 900
done
kc -n sandbox rollout status deployment/checkout --timeout=180s
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-mcp.py"
wait_for_application mcp "" 300
if kc -n pagerless get deployment pagerless-server >/dev/null 2>&1; then
	# Environment variables from a ConfigMap are read only when a container starts.
	kc -n pagerless rollout restart deployment/pagerless-server
fi
wait_for_application pagerless-server "" 300
wait_for_application pagerless-executor "" 300

echo
echo "The local stack is up. The checkout Workload in the sandbox namespace is healthy."
echo "The observability stack is in the monitoring namespace. Open Grafana with:"
echo "  kubectl --context kind-${KIND_CLUSTER_NAME} -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80"
echo "Inject a failure with: make break SCENARIO=oom"
echo "Restore the Sandbox with: make reset"

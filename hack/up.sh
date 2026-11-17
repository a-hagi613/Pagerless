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

reused_cluster=0
if kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	reused_cluster=1
	echo "Reusing the kind cluster $KIND_CLUSTER_NAME."
else
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --wait 180s
fi

if [ "${PAGERLESS_LOCAL_IMAGES:-0}" = "1" ]; then
	images=("$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_EXECUTOR_IMAGE" "$PAGERLESS_AGENT_IMAGE")
	if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
		images+=("$PAGERLESS_E2E_ADAPTER_IMAGE")
	fi
	kind load docker-image --name "$KIND_CLUSTER_NAME" "${images[@]}"
fi

# Authentication Secrets must exist before the observability Application can create Alertmanager.
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" alerts

helm repo add argo "$ARGOCD_CHART_REPO" --force-update >/dev/null
helm upgrade --install argocd argo/argo-cd \
	--version "$ARGOCD_CHART_VERSION" \
	--namespace argocd --create-namespace \
	--values "$REPO_ROOT/deploy/argocd/values.yaml" \
	--kube-context "kind-${KIND_CLUSTER_NAME}" \
	--wait --timeout 5m

if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	kc -n argocd create configmap argocd-tls-certs-cm \
		--from-file="$PAGERLESS_E2E_GIT_HOST=$PAGERLESS_E2E_CA_FILE" \
		--dry-run=client -o yaml |
		kc label --local -f - app.kubernetes.io/part-of=argocd -o yaml |
		kc apply -f -
fi

kc create namespace pagerless-system --dry-run=client -o yaml | kc apply -f -
kc create namespace pagerless --dry-run=client -o yaml | kc apply -f -
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	kc create namespace pagerless-e2e --dry-run=client -o yaml | kc apply -f -
fi

if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	kc -n pagerless create configmap pagerless-e2e-ca \
		--from-file=ca.crt="$PAGERLESS_E2E_CA_FILE" \
		--dry-run=client -o yaml | kc apply -f -
fi

# Secrets from .env. Tokens reach kubectl through file descriptors, not argv.
kc -n pagerless create secret generic github-read \
	--from-file=token=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create secret generic github-write \
	--from-file=token=<(printf '%s' "$GITHUB_WRITE_TOKEN") \
	--dry-run=client -o yaml | kc apply -f -
kc -n pagerless create configmap pagerless-sandbox \
	--from-literal=url="$SANDBOX_URL" \
	--from-literal=githubRepository="${PAGERLESS_GITHUB_REPOSITORY:-}" \
	--dry-run=client -o yaml | kc apply -f -
kc -n argocd create secret generic sandbox-repository \
	--from-literal=type=git \
	--from-literal=url="$SANDBOX_URL" \
	--from-literal=username=pagerless \
	--from-file=password=<(printf '%s' "$GITHUB_READ_TOKEN") \
	--dry-run=client -o yaml |
	kc label --local -f - argocd.argoproj.io/secret-type=repository -o yaml |
	kc apply -f -

if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	# The adapter reads its certificate, key, and tokens only when it starts.
	# A checksum on the pod template makes a reused cluster replace the pod
	# whenever the generated TLS material or credentials change, instead of
	# leaving it with values that no longer match the rest of the stack.
	adapter_checksum="$({
		cat "$PAGERLESS_E2E_CA_FILE" "$PAGERLESS_E2E_TLS_CERT_FILE" "$PAGERLESS_E2E_TLS_KEY_FILE"
		printf '%s' "$GITHUB_READ_TOKEN"
		printf '%s' "$GITHUB_WRITE_TOKEN"
	} | sha256sum | cut -d ' ' -f 1)"
	kc -n pagerless-e2e create secret tls sandbox-adapter-tls \
		--cert="$PAGERLESS_E2E_TLS_CERT_FILE" --key="$PAGERLESS_E2E_TLS_KEY_FILE" \
		--dry-run=client -o yaml | kc apply -f -
	kc -n pagerless-e2e create secret generic sandbox-adapter-token \
		--from-file=read-token=<(printf '%s' "$GITHUB_READ_TOKEN") \
		--from-file=write-token=<(printf '%s' "$GITHUB_WRITE_TOKEN") \
		--dry-run=client -o yaml | kc apply -f -
	kc apply -f "$REPO_ROOT/evals/e2e/manifests/adapter.yaml"
	kc -n pagerless-e2e patch deployment sandbox-adapter --type=merge \
		--patch "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"pagerless.dev/config-checksum\":\"$adapter_checksum\"}}}}}"
	kc -n pagerless-e2e rollout status deployment/sandbox-adapter --timeout=180s
	if [ "$reused_cluster" = 1 ]; then
		# Argo CD caches the Sandbox repository certificate and credential. A
		# reused cluster has just replaced both, so the repo server must reload
		# them before the Sandbox Application can sync.
		kc -n argocd rollout restart deployment/argocd-repo-server
		kc -n argocd rollout status deployment/argocd-repo-server --timeout=180s
	fi
fi

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
for app in kube-prometheus-stack loki tempo opentelemetry-collector observability-alerts \
	observability-monitors observability-dashboards; do
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
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" approver

echo
echo "The local stack is up. The checkout Workload in the sandbox namespace is healthy."
echo "The observability stack is in the monitoring namespace. Open Grafana with:"
echo "  kubectl --context kind-${KIND_CLUSTER_NAME} -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80"
echo "Inject a failure with: make break SCENARIO=oom"
echo "Restore the Sandbox with: make reset"

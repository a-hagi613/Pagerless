#!/usr/bin/env bash
# Deploys the Pagerless stack into a cluster that already exists and is
# reachable, whichever platform it runs on.
#
# `make up` creates a kind cluster and then calls this script; `make cloud-up`
# creates an EKS cluster and then calls the same script. Everything below the
# cluster is therefore identical on both, which is what keeps the local and
# cloud paths from drifting.
#
# The caller sets the context (PAGERLESS_KUBE_CONTEXT for EKS, otherwise the
# kind context) and the repository revision to sync.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

PLATFORM_REPO_URL="${PLATFORM_REPO_URL:-https://github.com/a-hagi613/pagerless}"
PLATFORM_REVISION="${PLATFORM_REVISION:-main}"
KUBE_CONTEXT="${PAGERLESS_KUBE_CONTEXT:-kind-${KIND_CLUSTER_NAME}}"

require_commands helm kubectl uv
require_env SANDBOX_URL GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN

# Authentication Secrets must exist before the observability Application can create Alertmanager.
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" alerts

helm repo add argo "$ARGOCD_CHART_REPO" --force-update >/dev/null
helm upgrade --install argocd argo/argo-cd \
	--version "$ARGOCD_CHART_VERSION" \
	--namespace argocd --create-namespace \
	--values "$REPO_ROOT/deploy/argocd/values.yaml" \
	--kube-context "$KUBE_CONTEXT" \
	--wait --timeout 15m

if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	kc -n argocd create configmap argocd-tls-certs-cm \
		--from-file="$PAGERLESS_E2E_GIT_HOST=$PAGERLESS_E2E_CA_FILE" \
		--dry-run=client -o yaml |
		kc label --local -f - app.kubernetes.io/part-of=argocd -o yaml |
		kc apply -f -
fi

# Argo CD treats an OCI Helm chart as a repository, and only a registered
# repository with enableOCI lets it pull the agentgateway charts.
kc apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: agentgateway-oci
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  type: helm
  name: agentgateway
  url: cr.agentgateway.dev/charts
  enableOCI: "true"
EOF

kc create namespace pagerless-system --dry-run=client -o yaml | kc apply -f -
kc create namespace pagerless --dry-run=client -o yaml | kc apply -f -
kc create namespace keycloak --dry-run=client -o yaml | kc apply -f -
kc create namespace agentgateway-system --dry-run=client -o yaml | kc apply -f -
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	kc create namespace pagerless-e2e --dry-run=client -o yaml | kc apply -f -
	kc -n pagerless create configmap pagerless-e2e-ca \
		--from-file=ca.crt="$PAGERLESS_E2E_CA_FILE" \
		--dry-run=client -o yaml | kc apply -f -
fi

# The Keycloak master-realm admin and database passwords are generated once and
# reused, because KC_BOOTSTRAP_ADMIN_PASSWORD applies only to a fresh database.
keycloak_admin_password="$(kc -n keycloak get secret keycloak-credentials \
	-o jsonpath='{.data.admin-password}' 2>/dev/null | base64 --decode || true)"
keycloak_database_password="$(kc -n keycloak get secret keycloak-credentials \
	-o jsonpath='{.data.database-password}' 2>/dev/null | base64 --decode || true)"
[ -n "$keycloak_admin_password" ] || keycloak_admin_password="$(openssl rand -hex 32)"
[ -n "$keycloak_database_password" ] || keycloak_database_password="$(openssl rand -hex 32)"
kc -n keycloak create secret generic keycloak-credentials \
	--from-file=admin-password=<(printf '%s' "$keycloak_admin_password") \
	--from-file=database-password=<(printf '%s' "$keycloak_database_password") \
	--dry-run=client -o yaml | kc apply -f -
unset keycloak_admin_password keycloak_database_password

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
	if [ "${PAGERLESS_REUSED_CLUSTER:-0}" = "1" ]; then
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
# The timeouts are generous because a cloud session pulls its images for the
# first time; a kind run finishes well inside them.
wait_for_application root "" 600 synced
wait_for_application gateway-api-crds "" 300 synced
wait_for_application sandbox "" 1200
for app in chaos-mesh kube-prometheus-stack loki tempo opentelemetry-collector observability-alerts \
	observability-monitors observability-dashboards; do
	wait_for_application "$app" "" 1800
done
kc -n sandbox rollout status deployment/checkout --timeout=900s
wait_for_application keycloak "" 900
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-keycloak.py"
for app in agentgateway-crds agentgateway; do
	wait_for_application "$app" "" 600 synced
done
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-mcp.py"
# The gateway routes wait for the caller credentials that provisioning writes.
if kc -n agentgateway-system get deployment mcp-gateway >/dev/null 2>&1; then
	kc -n agentgateway-system rollout restart deployment/mcp-gateway
fi
wait_for_application mcp-gateway "" 600 synced
wait_for_application mcp "" 600
if kc -n pagerless get deployment pagerless-server >/dev/null 2>&1; then
	# Environment variables from a ConfigMap are read only when a container starts.
	kc -n pagerless rollout restart deployment/pagerless-server
fi
wait_for_application pagerless-server "" 900
wait_for_application pagerless-executor "" 900
uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/hack/provision-authorization.py" approver

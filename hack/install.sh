#!/usr/bin/env bash
# Installs the standalone Pagerless operator and investigation stack into any
# Kubernetes cluster without deploying the OpenTelemetry shopping-cart demo,
# Chaos Mesh, or the Argo CD app-of-apps bootstrap.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions
load_env

DRY_RUN=0
ASSUME_YES=0
KUBE_CONTEXT="${PAGERLESS_KUBE_CONTEXT:-}"
WORKLOAD_NAMESPACES="${WORKLOAD_NAMESPACES:-default}"
INSTALL_PROVIDER="${MODEL_PROVIDER:-}"
INSTALL_SANDBOX_URL="${SANDBOX_URL:-}"
INSTALL_MCP_SERVERS="${PAGERLESS_MCP_SERVERS:-}"
INSTALL_ARGOCD_APP="${ARGOCD_APPLICATION:-${PAGERLESS_ARGOCD_APPLICATION:-}}"
INSTALL_GRAFANA_URL="${GRAFANA_URL:-http://kube-prometheus-stack-grafana.monitoring.svc}"

usage() {
	cat <<'EOF'
Usage: bash hack/install.sh [options]

Install the standalone Pagerless operator, Keycloak realm, agentgateway, MCP
servers, and per-namespace RBAC into any Kubernetes cluster.

Options:
  --dry-run                Render and print the installation plan and manifests
                           without contacting a cluster.
  --context <ctx>          Kubernetes context to target (defaults to
                           $PAGERLESS_KUBE_CONTEXT or current-context).
  --namespaces <ns1,ns2>   Comma-separated Workload namespaces to watch and
                           remediate (default: default).
  --provider <name>        Model provider: anthropic, bedrock, or replay
                           (default: anthropic when ANTHROPIC_API_KEY is set,
                           bedrock when AWS_REGION is set, otherwise anthropic).
  --sandbox-url <url>      Optional GitOps repository HTTPS URL for pull-request
                           remediations (PatchResources and Rollback). Omit for
                           live-only clusters (RestartWorkload and NoAction).
  --mcp-servers <csv>      Comma-separated MCP servers to enable from:
                           k8s,grafana,argocd,github (k8s is always required).
  --argocd-app <app>       Optional Argo CD Application name when Argo CD
                           manages the target Workloads.
  --grafana-url <url>      In-cluster Grafana base URL for mcp-grafana
                           (default: http://kube-prometheus-stack-grafana.monitoring.svc).
  --yes, -y                Proceed without interactive confirmation.
  --help, -h               Show this help text.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--context)
		[ "$#" -ge 2 ] || die "--context requires a value."
		KUBE_CONTEXT="$2"
		shift 2
		;;
	--context=*)
		KUBE_CONTEXT="${1#*=}"
		shift
		;;
	--namespaces)
		[ "$#" -ge 2 ] || die "--namespaces requires a comma-separated list."
		WORKLOAD_NAMESPACES="$2"
		shift 2
		;;
	--namespaces=*)
		WORKLOAD_NAMESPACES="${1#*=}"
		shift
		;;
	--provider)
		[ "$#" -ge 2 ] || die "--provider requires anthropic, bedrock, or replay."
		INSTALL_PROVIDER="$2"
		shift 2
		;;
	--provider=*)
		INSTALL_PROVIDER="${1#*=}"
		shift
		;;
	--sandbox-url)
		[ "$#" -ge 2 ] || die "--sandbox-url requires an HTTPS repository URL."
		INSTALL_SANDBOX_URL="$2"
		shift 2
		;;
	--sandbox-url=*)
		INSTALL_SANDBOX_URL="${1#*=}"
		shift
		;;
	--mcp-servers)
		[ "$#" -ge 2 ] || die "--mcp-servers requires a comma-separated list."
		INSTALL_MCP_SERVERS="$2"
		shift 2
		;;
	--mcp-servers=*)
		INSTALL_MCP_SERVERS="${1#*=}"
		shift
		;;
	--argocd-app)
		[ "$#" -ge 2 ] || die "--argocd-app requires an Application name."
		INSTALL_ARGOCD_APP="$2"
		shift 2
		;;
	--argocd-app=*)
		INSTALL_ARGOCD_APP="${1#*=}"
		shift
		;;
	--grafana-url)
		[ "$#" -ge 2 ] || die "--grafana-url requires an HTTP(S) URL."
		INSTALL_GRAFANA_URL="$2"
		shift 2
		;;
	--grafana-url=*)
		INSTALL_GRAFANA_URL="${1#*=}"
		shift
		;;
	--yes | -y)
		ASSUME_YES=1
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1 (run with --help for usage)"
		;;
	esac
done

require_commands helm kubectl openssl

if [ -z "$INSTALL_PROVIDER" ]; then
	if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
		INSTALL_PROVIDER="anthropic"
	elif [ -n "${AWS_REGION:-}" ]; then
		INSTALL_PROVIDER="bedrock"
	else
		INSTALL_PROVIDER="anthropic"
	fi
fi

case "$INSTALL_PROVIDER" in
anthropic | bedrock | replay) ;;
*) die "--provider must be anthropic, bedrock, or replay (got '$INSTALL_PROVIDER')." ;;
esac

# Parse and validate WORKLOAD_NAMESPACES as DNS-1123 labels.
IFS=',' read -r -a raw_namespaces <<<"$WORKLOAD_NAMESPACES"
NAMESPACES=()
for ns in "${raw_namespaces[@]}"; do
	ns="${ns#"${ns%%[![:space:]]*}"}"
	ns="${ns%"${ns##*[![:space:]]}"}"
	[ -n "$ns" ] || continue
	if ! [[ "$ns" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || [ "${#ns}" -gt 63 ]; then
		die "invalid namespace '$ns': must be a lowercase DNS-1123 label."
	fi
	duplicate=0
	for existing in "${NAMESPACES[@]:-}"; do
		if [ "$existing" = "$ns" ]; then
			duplicate=1
			break
		fi
	done
	if [ "$duplicate" -eq 0 ]; then
		NAMESPACES+=("$ns")
	fi
done
[ "${#NAMESPACES[@]}" -gt 0 ] || die "at least one Workload namespace is required."
WORKLOAD_NAMESPACES="$(IFS=','; echo "${NAMESPACES[*]}")"

# Compute or validate MCP_SERVERS.
if [ -z "$INSTALL_MCP_SERVERS" ]; then
	INSTALL_MCP_SERVERS="k8s,grafana"
	if [ -n "$INSTALL_ARGOCD_APP" ]; then
		INSTALL_MCP_SERVERS="${INSTALL_MCP_SERVERS},argocd"
	fi
	if [ -n "$INSTALL_SANDBOX_URL" ]; then
		INSTALL_MCP_SERVERS="${INSTALL_MCP_SERVERS},github"
	fi
fi

IFS=',' read -r -a raw_servers <<<"$INSTALL_MCP_SERVERS"
MCP_SERVERS=()
has_k8s=0
has_github=0
for srv in "${raw_servers[@]}"; do
	srv="${srv#"${srv%%[![:space:]]*}"}"
	srv="${srv%"${srv##*[![:space:]]}"}"
	[ -n "$srv" ] || continue
	case "$srv" in
	k8s | grafana | argocd | github) ;;
	*) die "unsupported MCP server '$srv' in --mcp-servers (expected k8s, grafana, argocd, or github)." ;;
	esac
	[ "$srv" = "k8s" ] && has_k8s=1
	[ "$srv" = "github" ] && has_github=1
	duplicate=0
	for existing in "${MCP_SERVERS[@]:-}"; do
		if [ "$existing" = "$srv" ]; then
			duplicate=1
			break
		fi
	done
	if [ "$duplicate" -eq 0 ]; then
		MCP_SERVERS+=("$srv")
	fi
done
[ "$has_k8s" -eq 1 ] || die "--mcp-servers must include 'k8s'."
INSTALL_MCP_SERVERS="$(IFS=','; echo "${MCP_SERVERS[*]}")"

SANDBOX_OWNER="${PAGERLESS_SANDBOX_OWNER:-}"
SANDBOX_REPO="${PAGERLESS_SANDBOX_REPO:-}"
if [ -n "$INSTALL_SANDBOX_URL" ]; then
	INSTALL_SANDBOX_URL="${INSTALL_SANDBOX_URL%/}"
	INSTALL_SANDBOX_URL="${INSTALL_SANDBOX_URL%.git}"
	if [ -z "$SANDBOX_OWNER" ] && [ -z "$SANDBOX_REPO" ]; then
		slug="${INSTALL_SANDBOX_URL#https://github.com/}"
		if [[ "$slug" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
			SANDBOX_OWNER="${slug%%/*}"
			SANDBOX_REPO="${slug##*/}"
		fi
	fi
fi

if [ "$has_github" -eq 1 ] && [ "$DRY_RUN" -eq 0 ]; then
	if [ -z "$SANDBOX_OWNER" ] || [ -z "$SANDBOX_REPO" ]; then
		die "GitHub MCP is enabled; provide --sandbox-url https://github.com/<owner>/<repo> or set PAGERLESS_SANDBOX_OWNER and PAGERLESS_SANDBOX_REPO."
	fi
fi

if [ -z "$KUBE_CONTEXT" ]; then
	if [ "$DRY_RUN" -eq 1 ]; then
		KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || echo "current-context")"
	else
		KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
		[ -n "$KUBE_CONTEXT" ] || die "no active kubectl context found; pass --context <ctx>."
	fi
fi

kctl() {
	kubectl --context "$KUBE_CONTEXT" "$@"
}

render_namespace_rbac() {
	local ns
	for ns in "${NAMESPACES[@]}"; do
		cat <<EOF
---
apiVersion: v1
kind: Namespace
metadata:
  name: "${ns}"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pagerless-server-validation
  namespace: "${ns}"
rules:
  - apiGroups: [apps]
    resources: [deployments, statefulsets]
    verbs: [get, list, update]
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: pagerless-server-validation
  namespace: "${ns}"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pagerless-server-validation
subjects:
  - kind: ServiceAccount
    name: pagerless-server
    namespace: pagerless
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pagerless-executor-restart
  namespace: "${ns}"
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list, watch, delete]
  - apiGroups: [apps]
    resources: [deployments, statefulsets]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: pagerless-executor-restart
  namespace: "${ns}"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pagerless-executor-restart
subjects:
  - kind: ServiceAccount
    name: pagerless-executor
    namespace: pagerless
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pagerless-mcp-read
  namespace: "${ns}"
rules:
  - apiGroups: [""]
    resources: [pods, events]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, replicasets]
    verbs: [get, list, watch]
  - apiGroups: [events.k8s.io]
    resources: [events]
    verbs: [get, list, watch]
  - apiGroups: [chaos-mesh.org]
    resources: [stresschaos]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: pagerless-mcp-read
  namespace: "${ns}"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pagerless-mcp-read
subjects:
  - kind: ServiceAccount
    name: mcp-kubernetes
    namespace: pagerless
EOF
	done
}

render_configmaps() {
	cat <<EOF
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: pagerless-sandbox
  namespace: pagerless
data:
  url: "${INSTALL_SANDBOX_URL}"
  githubRepository: "${PAGERLESS_GITHUB_REPOSITORY:-}"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: pagerless-agent-config
  namespace: pagerless
data:
  PAGERLESS_MODEL_PROVIDER: "${INSTALL_PROVIDER}"
  PAGERLESS_MCP_GATEWAY_URL: "http://mcp-gateway.agentgateway-system.svc.cluster.local"
  PAGERLESS_MCP_SERVERS: "${INSTALL_MCP_SERVERS}"
  PAGERLESS_KEYCLOAK_ISSUER: "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless"
  PAGERLESS_KEYCLOAK_TOKEN_URL: "http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless/protocol/openid-connect/token"
  PAGERLESS_KEYCLOAK_CLIENT_ID: "pagerless-agent"
  PAGERLESS_REPLAY_FILE: "${PAGERLESS_REPLAY_FILE:-/app/recordings/oom.json}"
  PAGERLESS_NARRATIVE_REPLAY_FILE: "${PAGERLESS_NARRATIVE_REPLAY_FILE:-/app/recordings/narrative.json}"
  PAGERLESS_SANDBOX_OWNER: "${SANDBOX_OWNER}"
  PAGERLESS_SANDBOX_REPO: "${SANDBOX_REPO}"
  PAGERLESS_ARGOCD_APPLICATION: "${INSTALL_ARGOCD_APP}"
  PAGERLESS_ALLOWED_NAMESPACES: "${WORKLOAD_NAMESPACES}"
  PAGERLESS_SANDBOX_URL: "${INSTALL_SANDBOX_URL}"
EOF
	if [ -n "${AWS_REGION:-}" ]; then
		printf '  AWS_REGION: "%s"\n' "$AWS_REGION"
	fi
}

render_mcp_manifests() {
	helm template mcp "$REPO_ROOT/deploy/mcp" \
		--namespace pagerless \
		--set "allowedNamespaces={${WORKLOAD_NAMESPACES}}" \
		--set-string "githubHost=${PAGERLESS_MCP_GITHUB_HOST:-}" |
		sed "s|http://kube-prometheus-stack-grafana.monitoring.svc|${INSTALL_GRAFANA_URL}|g"
}

render_core_manifests() {
	cat <<'EOF'
---
apiVersion: v1
kind: Namespace
metadata:
  name: pagerless
---
apiVersion: v1
kind: Namespace
metadata:
  name: keycloak
---
apiVersion: v1
kind: Namespace
metadata:
  name: agentgateway-system
EOF
	render_namespace_rbac
	render_configmaps
	echo "---"
	helm template keycloak "$REPO_ROOT/deploy/keycloak" --namespace keycloak
	echo "---"
	helm template mcp-gateway "$REPO_ROOT/deploy/gateway" --namespace agentgateway-system
	echo "---"
	render_mcp_manifests
	echo "---"
	kubectl kustomize "$REPO_ROOT/deploy/pagerless-server"
	echo "---"
	kubectl kustomize "$REPO_ROOT/deploy/pagerless-executor"
}

patch_server_deployment_json() {
	cat <<EOF
[
  {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": [
    "--agent-image=${PAGERLESS_AGENT_IMAGE}",
    "--report-url=http://pagerless-server.pagerless.svc:8080",
    "--metrics-bind-address=:9090",
    "--dashboard-bind-address=:8082",
    "--grafana-url=${BROWSER_GRAFANA_URL:-http://localhost:3000}",
    "--dashboard-base-url=${DASHBOARD_BASE_URL:-http://localhost:8080}",
    "--dashboard-cookie-secure=${DASHBOARD_COOKIE_SECURE:-false}",
    "--oidc-issuer=http://keycloak.keycloak.svc.cluster.local:8080/realms/pagerless",
    "--oidc-client-id=pagerless-dashboard",
    "--oidc-client-secret-file=/var/run/pagerless/keycloak/client-secret",
    "--keycloak-base-url=http://keycloak.keycloak.svc.cluster.local:8080",
    "--keycloak-admin-client-id=pagerless-server",
    "--keycloak-admin-client-secret-file=/var/run/pagerless/keycloak/server-client-secret",
    "--approvers-group=pagerless-approvers",
    "--workload-namespaces=${WORKLOAD_NAMESPACES}",
    "--alert-namespaces=${WORKLOAD_NAMESPACES}",
    "--patch-namespaces=${WORKLOAD_NAMESPACES}",
    "--argocd-application=${INSTALL_ARGOCD_APP:-sandbox}",
    "--sandbox-url=${INSTALL_SANDBOX_URL}",
    "--history-path=/var/lib/pagerless/history.db",
    "--executor-url=http://pagerless-executor.pagerless.svc:8080",
    "--argocd-url=${ARGOCD_SERVER_URL:-http://argocd-server.argocd.svc}",
    "--alertmanager-url=${ALERTMANAGER_URL:-http://kube-prometheus-stack-alertmanager.monitoring.svc:9093}",
    "--alertmanager-token-file=/var/run/pagerless/alertmanager/token",
    "--sandbox-read-token-file=/var/run/pagerless/github-read/token"
  ]}
]
EOF
}

if [ "$DRY_RUN" -eq 1 ]; then
	cat <<EOF
# Pagerless standalone installation plan (--dry-run)
# Context:             ${KUBE_CONTEXT}
# Workload namespaces: ${WORKLOAD_NAMESPACES}
# Model provider:      ${INSTALL_PROVIDER}
# MCP servers:         ${INSTALL_MCP_SERVERS}
# Sandbox GitOps URL:  ${INSTALL_SANDBOX_URL:-<none: live-only remediation mode>}
# Argo CD Application: ${INSTALL_ARGOCD_APP:-<none>}
# Grafana MCP URL:     ${INSTALL_GRAFANA_URL}
EOF
	render_core_manifests
	echo "# Planned deployment/pagerless-server JSON patch:"
	patch_server_deployment_json | sed 's/^/# /'
	exit 0
fi

require_commands uv
if [ "$INSTALL_PROVIDER" = "anthropic" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
	die "ANTHROPIC_API_KEY must be set in the environment or .env when --provider=anthropic."
fi
if [ "$INSTALL_PROVIDER" = "bedrock" ] && [ -z "${AWS_REGION:-}" ]; then
	die "AWS_REGION must be set in the environment or .env when --provider=bedrock."
fi
if [ -n "$INSTALL_SANDBOX_URL" ]; then
	[ -n "${GITHUB_READ_TOKEN:-}" ] || die "GITHUB_READ_TOKEN is required when --sandbox-url is set."
	[ -n "${GITHUB_WRITE_TOKEN:-}" ] || die "GITHUB_WRITE_TOKEN is required when --sandbox-url is set."
fi

kctl cluster-info >/dev/null 2>&1 || die "cannot reach Kubernetes cluster for context '$KUBE_CONTEXT'."

if [ "$ASSUME_YES" -eq 0 ]; then
	echo "Installing Pagerless into context '$KUBE_CONTEXT':"
	echo "  Workload namespaces: $WORKLOAD_NAMESPACES"
	echo "  Model provider:      $INSTALL_PROVIDER"
	echo "  MCP servers:         $INSTALL_MCP_SERVERS"
	echo "  Sandbox GitOps URL:  ${INSTALL_SANDBOX_URL:-<none (live-only mode)>}"
	echo "  Argo CD Application: ${INSTALL_ARGOCD_APP:-<none>}"
	read -r -p "Continue? [y/N] " confirm
	case "$confirm" in
	y | Y | yes | YES) ;;
	*) die "installation aborted." ;;
	esac
fi

# 1. Namespaces and CRDs.
for ns in pagerless keycloak agentgateway-system sandbox "${NAMESPACES[@]}"; do
	kctl create namespace "$ns" --dry-run=client -o yaml | kctl apply -f -
done

kctl apply -f "$REPO_ROOT/deploy/pagerless-server/crds/pagerless.dev_triages.yaml"
kctl apply -f "$REPO_ROOT/deploy/pagerless-server/crds/pagerless.dev_triageapprovals.yaml"
kctl apply --server-side -f "$REPO_ROOT/deploy/gateway-api-crds/standard-install.yaml"

# 2. Keycloak deployment and realm provisioning.
keycloak_admin_password="$(kctl -n keycloak get secret keycloak-credentials \
	-o jsonpath='{.data.admin-password}' 2>/dev/null | base64 --decode || true)"
keycloak_database_password="$(kctl -n keycloak get secret keycloak-credentials \
	-o jsonpath='{.data.database-password}' 2>/dev/null | base64 --decode || true)"
[ -n "$keycloak_admin_password" ] || keycloak_admin_password="$(openssl rand -hex 32)"
[ -n "$keycloak_database_password" ] || keycloak_database_password="$(openssl rand -hex 32)"
kctl -n keycloak create secret generic keycloak-credentials \
	--from-file=admin-password=<(printf '%s' "$keycloak_admin_password") \
	--from-file=database-password=<(printf '%s' "$keycloak_database_password") \
	--dry-run=client -o yaml | kctl apply -f -
unset keycloak_admin_password keycloak_database_password

helm template keycloak "$REPO_ROOT/deploy/keycloak" --namespace keycloak |
	kctl -n keycloak apply -f -
kctl -n keycloak rollout status statefulset/keycloak-db --timeout=300s
kctl -n keycloak rollout status deployment/keycloak --timeout=600s

PAGERLESS_KUBE_CONTEXT="$KUBE_CONTEXT" uv run --no-project --python "$PYTHON_VERSION" -c '
import importlib.util, os, sys
spec = importlib.util.spec_from_file_location("provision_keycloak", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.CONTEXT = os.environ["PAGERLESS_KUBE_CONTEXT"]
module.main()
' "$REPO_ROOT/hack/provision-keycloak.py"

# 3. Agentgateway CRDs, controller, and MCP gateway routes/policies.
helm upgrade --install agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
	--version v1.7.0 \
	--namespace agentgateway-system \
	--create-namespace \
	--kube-context "$KUBE_CONTEXT" \
	--wait --timeout 5m

helm upgrade --install agentgateway oci://cr.agentgateway.dev/charts/agentgateway \
	--version v1.7.0 \
	--namespace agentgateway-system \
	--values "$REPO_ROOT/deploy/agentgateway/values.yaml" \
	--kube-context "$KUBE_CONTEXT" \
	--wait --timeout 5m

# 4. Idempotent Secrets and ConfigMaps.
PAGERLESS_KUBE_CONTEXT="$KUBE_CONTEXT" uv run --no-project --python "$PYTHON_VERSION" \
	"$REPO_ROOT/hack/provision-authorization.py" alerts

kctl -n pagerless create secret generic github-read \
	--from-file=token=<(printf '%s' "${GITHUB_READ_TOKEN:-}") \
	--dry-run=client -o yaml | kctl apply -f -
kctl -n pagerless create secret generic github-write \
	--from-file=token=<(printf '%s' "${GITHUB_WRITE_TOKEN:-}") \
	--dry-run=client -o yaml | kctl apply -f -
kctl -n pagerless create secret generic argocd-read \
	--from-file=token=<(printf '%s' "${ARGOCD_AUTH_TOKEN:-}") \
	--dry-run=client -o yaml | kctl apply -f -
kctl -n pagerless create secret generic mcp-backend-tokens \
	--from-file=grafana=<(printf '%s' "${GRAFANA_SERVICE_ACCOUNT_TOKEN:-}") \
	--dry-run=client -o yaml | kctl apply -f -

grafana_caller="$(kctl -n pagerless get secret mcp-client-tokens \
	-o jsonpath='{.data.grafana}' 2>/dev/null | base64 --decode || true)"
argocd_caller="$(kctl -n pagerless get secret mcp-client-tokens \
	-o jsonpath='{.data.argocd}' 2>/dev/null | base64 --decode || true)"
[ -n "$grafana_caller" ] || grafana_caller="$(openssl rand -hex 32)"
[ -n "$argocd_caller" ] || argocd_caller="$(openssl rand -hex 32)"
for target_ns in pagerless agentgateway-system; do
	kctl -n "$target_ns" create secret generic mcp-client-tokens \
		--from-file=grafana=<(printf '%s' "$grafana_caller") \
		--from-file=argocd=<(printf '%s' "$argocd_caller") \
		--from-file=github=<(printf '%s' "${GITHUB_READ_TOKEN:-}") \
		--dry-run=client -o yaml | kctl apply -f -
done
unset grafana_caller argocd_caller

agent_cred_args=()
if [ "$INSTALL_PROVIDER" = "anthropic" ] && [ -n "${ANTHROPIC_API_KEY:-}" ]; then
	agent_cred_args+=(--from-file=ANTHROPIC_API_KEY=<(printf '%s' "$ANTHROPIC_API_KEY"))
elif [ "$INSTALL_PROVIDER" = "bedrock" ]; then
	[ -n "${AWS_ACCESS_KEY_ID:-}" ] && agent_cred_args+=(--from-file=AWS_ACCESS_KEY_ID=<(printf '%s' "$AWS_ACCESS_KEY_ID"))
	[ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && agent_cred_args+=(--from-file=AWS_SECRET_ACCESS_KEY=<(printf '%s' "$AWS_SECRET_ACCESS_KEY"))
	[ -n "${AWS_SESSION_TOKEN:-}" ] && agent_cred_args+=(--from-file=AWS_SESSION_TOKEN=<(printf '%s' "$AWS_SESSION_TOKEN"))
fi
kctl -n pagerless create secret generic pagerless-agent-credentials \
	"${agent_cred_args[@]}" \
	--dry-run=client -o yaml | kctl apply -f -

render_configmaps | kctl apply -f -

# 5. MCP gateway, MCP servers, and per-namespace RBAC.
helm template mcp-gateway "$REPO_ROOT/deploy/gateway" --namespace agentgateway-system |
	kctl -n agentgateway-system apply -f -
if kctl -n agentgateway-system get deployment mcp-gateway >/dev/null 2>&1; then
	kctl -n agentgateway-system rollout restart deployment/mcp-gateway
	kctl -n agentgateway-system rollout status deployment/mcp-gateway --timeout=300s
fi

render_mcp_manifests | kctl -n pagerless apply -f -

render_namespace_rbac | kctl apply -f -

# 6. Pagerless control plane and Executor.
kubectl kustomize "$REPO_ROOT/deploy/pagerless-server" | kctl apply -f -
kubectl kustomize "$REPO_ROOT/deploy/pagerless-executor" | kctl apply -f -

kctl -n pagerless patch deployment pagerless-server --type=json \
	--patch "$(patch_server_deployment_json)"
kctl -n pagerless rollout status deployment/pagerless-server --timeout=300s
kctl -n pagerless rollout status deployment/pagerless-executor --timeout=300s

PAGERLESS_KUBE_CONTEXT="$KUBE_CONTEXT" uv run --no-project --python "$PYTHON_VERSION" \
	"$REPO_ROOT/hack/provision-authorization.py" approver || true

echo
echo "Pagerless is installed in context '$KUBE_CONTEXT' watching namespaces: $WORKLOAD_NAMESPACES"
echo "Open the dashboard with:"
echo "  kubectl --context $KUBE_CONTEXT -n pagerless port-forward svc/pagerless-server 8080:8082"
echo "Start a Triage with:"
echo "  kubectl triage --context $KUBE_CONTEXT --namespace ${NAMESPACES[0]} deploy/<workload-name>"

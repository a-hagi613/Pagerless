#!/usr/bin/env bash
# Installs Pagerless into a Kubernetes cluster with the charts/pagerless chart.
#
# Pagerless does not install a cluster's platform: an OIDC issuer, an MCP
# gateway, Prometheus, Grafana, and Argo CD are the operator's own services, and
# the chart references them through values. This script applies the CRDs, writes
# the agent credentials Secret from the environment, and runs helm upgrade
# --install with the values it derives from its arguments.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

DRY_RUN=0
ASSUME_YES=0
KUBE_CONTEXT="${PAGERLESS_KUBE_CONTEXT:-}"
WORKLOAD_NAMESPACES="${WORKLOAD_NAMESPACES:-default}"
INSTALL_PROVIDER="${MODEL_PROVIDER:-}"
INSTALL_SANDBOX_URL="${SANDBOX_URL:-}"
INSTALL_ARGOCD_APP="${ARGOCD_APPLICATION:-${PAGERLESS_ARGOCD_APPLICATION:-}}"
INSTALL_GRAFANA_URL="${GRAFANA_URL:-http://kube-prometheus-stack-grafana.monitoring.svc}"

RELEASE_NAME=pagerless
RELEASE_NAMESPACE=pagerless
CHART_DIR="$REPO_ROOT/charts/pagerless"

usage() {
	cat <<'EOF'
Usage: bash hack/install.sh [options]

Install the Pagerless operator, agent configuration, dashboard, and the RBAC and
admission policies the chart defines into an existing Kubernetes cluster.

Options:
  --dry-run                Render and print the installation plan and manifests
                           without contacting a cluster.
  --context <ctx>          Kubernetes context to target (defaults to
                           $PAGERLESS_KUBE_CONTEXT or current-context).
  --namespaces <ns1,ns2>   Comma-separated Workload namespaces to watch and
                           propose changes for (default: default).
  --provider <name>        Model provider: anthropic, bedrock, or replay
                           (default: anthropic when ANTHROPIC_API_KEY is set,
                           otherwise replay).
  --sandbox-url <url>      Optional GitOps repository HTTPS URL that pull
                           request remediations target. Omit for live-only mode.
  --argocd-app <app>       Optional Argo CD Application name when Argo CD
                           manages the target Workloads.
  --grafana-url <url>      In-cluster Grafana base URL the agent's Grafana tools
                           read (default: http://kube-prometheus-stack-grafana.monitoring.svc).
  --yes, -y                Proceed without interactive confirmation.
  --help, -h               Show this help text.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1; shift ;;
	--context) [ "$#" -ge 2 ] || die "--context requires a value."; KUBE_CONTEXT="$2"; shift 2 ;;
	--context=*) KUBE_CONTEXT="${1#*=}"; shift ;;
	--namespaces) [ "$#" -ge 2 ] || die "--namespaces requires a comma-separated list."; WORKLOAD_NAMESPACES="$2"; shift 2 ;;
	--namespaces=*) WORKLOAD_NAMESPACES="${1#*=}"; shift ;;
	--provider) [ "$#" -ge 2 ] || die "--provider requires anthropic, bedrock, or replay."; INSTALL_PROVIDER="$2"; shift 2 ;;
	--provider=*) INSTALL_PROVIDER="${1#*=}"; shift ;;
	--sandbox-url) [ "$#" -ge 2 ] || die "--sandbox-url requires an HTTPS repository URL."; INSTALL_SANDBOX_URL="$2"; shift 2 ;;
	--sandbox-url=*) INSTALL_SANDBOX_URL="${1#*=}"; shift ;;
	--argocd-app) [ "$#" -ge 2 ] || die "--argocd-app requires an Application name."; INSTALL_ARGOCD_APP="$2"; shift 2 ;;
	--argocd-app=*) INSTALL_ARGOCD_APP="${1#*=}"; shift ;;
	--grafana-url) [ "$#" -ge 2 ] || die "--grafana-url requires an HTTP(S) URL."; INSTALL_GRAFANA_URL="$2"; shift 2 ;;
	--grafana-url=*) INSTALL_GRAFANA_URL="${1#*=}"; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	--help | -h) usage; exit 0 ;;
	*) die "unknown argument: $1 (run with --help for usage)" ;;
	esac
done

require_commands helm kubectl

if [ -z "$INSTALL_PROVIDER" ]; then
	if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
		INSTALL_PROVIDER="anthropic"
	elif [ -n "${AWS_REGION:-}" ]; then
		INSTALL_PROVIDER="bedrock"
	else
		INSTALL_PROVIDER="replay"
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
		[ "$existing" = "$ns" ] && duplicate=1
	done
	[ "$duplicate" -eq 0 ] && NAMESPACES+=("$ns")
done
[ "${#NAMESPACES[@]}" -gt 0 ] || die "at least one Workload namespace is required."
WORKLOAD_NAMESPACES="$(IFS=','; echo "${NAMESPACES[*]}")"

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

if [ -z "$KUBE_CONTEXT" ]; then
	if [ "$DRY_RUN" -eq 1 ]; then
		KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || echo "current-context")"
	else
		KUBE_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
		[ -n "$KUBE_CONTEXT" ] || die "no active kubectl context found; pass --context <ctx>."
	fi
fi

# The rendered values are written to a private temporary directory and removed
# on exit. They hold configuration only; every credential stays in a Secret.
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-install.XXXXXXXX")"
chmod 700 "$WORK_DIR"
trap 'rm -rf "$WORK_DIR"' EXIT
CHART_VALUES="$WORK_DIR/values.yaml"

# Quotes one YAML string. Single quotes are doubled, so a value can never end
# the scalar or start a new key.
yaml_string() {
	printf "'%s'" "${1//\'/\'\'}"
}

write_chart_values() {
	local ns
	cat >"$CHART_VALUES" <<EOF
workloadNamespaces:
$(for ns in "${NAMESPACES[@]}"; do printf '  - %s\n' "$(yaml_string "$ns")"; done)
agent:
  provider: $(yaml_string "$INSTALL_PROVIDER")
  awsRegion: $(yaml_string "${AWS_REGION:-}")
sandbox:
  url: $(yaml_string "$INSTALL_SANDBOX_URL")
  branch: $(yaml_string "${SANDBOX_BRANCH:-main}")
  owner: $(yaml_string "$SANDBOX_OWNER")
  repo: $(yaml_string "$SANDBOX_REPO")
argocd:
  application: $(yaml_string "$INSTALL_ARGOCD_APP")
  url: $(yaml_string "${ARGOCD_SERVER_URL:-http://argocd-server.argocd.svc}")
alertmanager:
  url: $(yaml_string "${ALERTMANAGER_URL:-http://kube-prometheus-stack-alertmanager.monitoring.svc:9093}")
grafana:
  browserUrl: $(yaml_string "${BROWSER_GRAFANA_URL:-http://localhost:3000}")
  mcpUrl: $(yaml_string "$INSTALL_GRAFANA_URL")
dashboard:
  baseUrl: $(yaml_string "${DASHBOARD_BASE_URL:-http://localhost:8080}")
  cookieSecure: ${DASHBOARD_COOKIE_SECURE:-false}
EOF
}

write_chart_values

kctl() {
	kubectl --context "$KUBE_CONTEXT" "$@"
}

if [ "$DRY_RUN" -eq 1 ]; then
	cat <<EOF
# Pagerless installation plan (--dry-run)
# Context:             ${KUBE_CONTEXT}
# Workload namespaces: ${WORKLOAD_NAMESPACES}
# Model provider:      ${INSTALL_PROVIDER}
# Sandbox GitOps URL:  ${INSTALL_SANDBOX_URL:-<none: live-only mode>}
# Argo CD Application: ${INSTALL_ARGOCD_APP:-<none>}
EOF
	cat "$CHART_DIR"/crds/*.yaml
	echo "---"
	helm template "$RELEASE_NAME" "$CHART_DIR" \
		--namespace "$RELEASE_NAMESPACE" \
		--values "$CHART_VALUES"
	exit 0
fi

kctl cluster-info >/dev/null 2>&1 || die "cannot reach Kubernetes cluster for context '$KUBE_CONTEXT'."

if [ "$ASSUME_YES" -eq 0 ]; then
	echo "Installing Pagerless into context '$KUBE_CONTEXT':"
	echo "  Workload namespaces: $WORKLOAD_NAMESPACES"
	echo "  Model provider:      $INSTALL_PROVIDER"
	echo "  Sandbox GitOps URL:  ${INSTALL_SANDBOX_URL:-<none (live-only mode)>}"
	echo "  Argo CD Application: ${INSTALL_ARGOCD_APP:-<none>}"
	read -r -p "Continue? [y/N] " confirm
	case "$confirm" in
	y | Y | yes | YES) ;;
	*) die "installation aborted." ;;
	esac
fi

# 1. Namespaces and CRDs. The release namespace is created by Helm; the CRDs are
# applied explicitly so that a chart upgrade does not own their lifecycle.
kctl create namespace "$RELEASE_NAMESPACE" --dry-run=client -o yaml | kctl apply -f -
kctl apply -f "$CHART_DIR/crds/pagerless.dev_triages.yaml"
kctl apply -f "$CHART_DIR/crds/pagerless.dev_triageapprovals.yaml"

# 2. Idempotent credentials. They exist before the release starts, so every pod
# that mounts one can be scheduled by the chart's first rollout.
agent_cred_args=()
if [ "$INSTALL_PROVIDER" = "anthropic" ] && [ -n "${ANTHROPIC_API_KEY:-}" ]; then
	agent_cred_args+=(--from-file=ANTHROPIC_API_KEY=<(printf '%s' "$ANTHROPIC_API_KEY"))
elif [ "$INSTALL_PROVIDER" = "bedrock" ]; then
	[ -n "${AWS_ACCESS_KEY_ID:-}" ] && agent_cred_args+=(--from-file=AWS_ACCESS_KEY_ID=<(printf '%s' "$AWS_ACCESS_KEY_ID"))
	[ -n "${AWS_SECRET_ACCESS_KEY:-}" ] && agent_cred_args+=(--from-file=AWS_SECRET_ACCESS_KEY=<(printf '%s' "$AWS_SECRET_ACCESS_KEY"))
	[ -n "${AWS_SESSION_TOKEN:-}" ] && agent_cred_args+=(--from-file=AWS_SESSION_TOKEN=<(printf '%s' "$AWS_SESSION_TOKEN"))
fi
kctl -n "$RELEASE_NAMESPACE" create secret generic pagerless-agent-credentials \
	${agent_cred_args[@]+"${agent_cred_args[@]}"} \
	--dry-run=client -o yaml | kctl apply -f -

if [ -n "$INSTALL_SANDBOX_URL" ]; then
	[ -n "${GITHUB_READ_TOKEN:-}" ] || die "GITHUB_READ_TOKEN is required when --sandbox-url is set."
	[ -n "${GITHUB_WRITE_TOKEN:-}" ] || die "GITHUB_WRITE_TOKEN is required when --sandbox-url is set."
	kctl -n "$RELEASE_NAMESPACE" create secret generic github-read \
		--from-file=token=<(printf '%s' "$GITHUB_READ_TOKEN") \
		--dry-run=client -o yaml | kctl apply -f -
	kctl -n "$RELEASE_NAMESPACE" create secret generic github-write \
		--from-file=token=<(printf '%s' "$GITHUB_WRITE_TOKEN") \
		--dry-run=client -o yaml | kctl apply -f -
fi

# 3. The release. The chart renders the exact container flags, so no post-render
# patch is needed.
helm upgrade --install "$RELEASE_NAME" "$CHART_DIR" \
	--namespace "$RELEASE_NAMESPACE" \
	--kube-context "$KUBE_CONTEXT" \
	--values "$CHART_VALUES" \
	--skip-crds

kctl -n "$RELEASE_NAMESPACE" rollout status deployment/pagerless-server --timeout=300s

echo
echo "Pagerless is installed in context '$KUBE_CONTEXT' watching namespaces: $WORKLOAD_NAMESPACES"
echo "Open the dashboard with:"
echo "  kubectl --context $KUBE_CONTEXT -n $RELEASE_NAMESPACE port-forward svc/pagerless-server 8080:8082"
echo "Start a Triage with:"
echo "  kubectl triage --context $KUBE_CONTEXT --namespace ${NAMESPACES[0]} deploy/<workload-name>"

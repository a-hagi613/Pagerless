#!/usr/bin/env bash
# Installs Pagerless into a Kubernetes cluster from charts/pagerless.
#
# The chart is standalone: it needs no database, identity provider, or gateway.
# This script only runs `helm upgrade --install`; everything it changes is in the
# release namespace, the read-only ClusterRole, and the CRDs.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

DRY_RUN=0
ASSUME_YES=0
KUBE_CONTEXT="${PAGERLESS_KUBE_CONTEXT:-}"
WATCH_NAMESPACES=""

RELEASE_NAME=pagerless
RELEASE_NAMESPACE=pagerless
CHART_DIR="$REPO_ROOT/charts/pagerless"

usage() {
	cat <<'EOF'
Usage: bash hack/install.sh [options]

Install Pagerless into an existing Kubernetes cluster.

Options:
  --dry-run                Render the installation without contacting a cluster.
  --context <ctx>          Kubernetes context to target (defaults to
                           $PAGERLESS_KUBE_CONTEXT or current-context).
  --namespaces <ns1,ns2>   Watch only these namespaces instead of every
                           namespace. The chart renders one read Role per entry.
  --yes, -y                Proceed without interactive confirmation.
  --help, -h               Show this help text.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--dry-run) DRY_RUN=1; shift ;;
	--context) [ "$#" -ge 2 ] || die "--context requires a value."; KUBE_CONTEXT="$2"; shift 2 ;;
	--context=*) KUBE_CONTEXT="${1#*=}"; shift ;;
	--namespaces) [ "$#" -ge 2 ] || die "--namespaces requires a comma-separated list."; WATCH_NAMESPACES="$2"; shift 2 ;;
	--namespaces=*) WATCH_NAMESPACES="${1#*=}"; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	--help | -h) usage; exit 0 ;;
	*) die "unknown argument: $1 (run with --help for usage)" ;;
	esac
done

require_commands helm
if [ "$DRY_RUN" -eq 0 ]; then
	require_commands kubectl
fi

ARGS=(upgrade --install "$RELEASE_NAME" "$CHART_DIR" --namespace "$RELEASE_NAMESPACE" --create-namespace)
if [ -n "$KUBE_CONTEXT" ]; then
	ARGS+=(--kube-context "$KUBE_CONTEXT")
fi
if [ -n "$WATCH_NAMESPACES" ]; then
	ARGS+=(--set rbac.watchAllNamespaces=false --set "rbac.watchNamespaces={$WATCH_NAMESPACES}")
fi

if [ "$DRY_RUN" -eq 1 ]; then
	TEMPLATE_ARGS=(template "$RELEASE_NAME" "$CHART_DIR" --namespace "$RELEASE_NAMESPACE")
	if [ -n "$WATCH_NAMESPACES" ]; then
		TEMPLATE_ARGS+=(--set rbac.watchAllNamespaces=false --set "rbac.watchNamespaces={$WATCH_NAMESPACES}")
	fi
	helm "${TEMPLATE_ARGS[@]}"
	exit 0
fi

echo "Pagerless will be installed into namespace $RELEASE_NAMESPACE."
if [ -n "$WATCH_NAMESPACES" ]; then
	echo "It will watch only: $WATCH_NAMESPACES"
else
	echo "It will watch every namespace through a read-only ClusterRole."
fi
echo "Its cluster identity cannot write to any workload, Event, or Node."
if [ "$ASSUME_YES" -eq 0 ]; then
	read -r -p "Install now? [y/N] " answer
	case "$answer" in
	y | Y | yes | YES) ;;
	*) die "not installed: the answer was not yes." ;;
	esac
fi

helm "${ARGS[@]}"
echo
echo "Pagerless is installed. Fill Secret $RELEASE_NAMESPACE/pagerless-credentials, then:"
echo "  kubectl -n $RELEASE_NAMESPACE port-forward svc/pagerless-server 8080:8080"

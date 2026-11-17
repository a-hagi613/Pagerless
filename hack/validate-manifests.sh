#!/usr/bin/env bash
# Renders every Helm chart and kustomization in the repository and validates the
# result with the pinned kubeconform. Custom resources from the Gateway API and
# agentgateway CRDs have no schema in kubeconform's default catalog, so those
# kinds are skipped; their CRDs, and every core Kubernetes object, are checked.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands helm kubeconform kubectl
case "$(helm version --short 2>/dev/null)" in
v"${HELM_VERSION}"* | "${HELM_VERSION}"*) ;;
*) die "helm v${HELM_VERSION} is required to validate the manifests." ;;
esac
[ "$(kubeconform -v 2>/dev/null)" = "$KUBECONFORM_VERSION" ] ||
	die "kubeconform $KUBECONFORM_VERSION is required to validate the manifests."

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-manifests.XXXXXXXX")"
chmod 700 "$state"
trap 'rm -rf "$state"' EXIT

helm template pagerless-root "$REPO_ROOT/deploy/root" \
	--namespace argocd \
	--set "platform.repoURL=https://github.com/a-hagi613/pagerless" \
	--set "platform.targetRevision=main" \
	--set "sandbox.repoURL=https://github.com/a-hagi613/pagerless-sandbox" >"$state/root.yaml"

helm template pagerless-apps "$REPO_ROOT/deploy/apps" \
	--namespace argocd \
	--set "platform.repoURL=https://github.com/a-hagi613/pagerless" \
	--set "platform.targetRevision=main" \
	--set "sandbox.repoURL=https://github.com/a-hagi613/pagerless-sandbox" \
	--set-string "mcp.githubHost=" >"$state/apps.yaml"

helm template mcp "$REPO_ROOT/deploy/mcp" \
	--namespace pagerless \
	--set-string "githubHost=" >"$state/mcp.yaml"

helm template keycloak "$REPO_ROOT/deploy/keycloak" \
	--namespace keycloak >"$state/keycloak.yaml"

helm template mcp-gateway "$REPO_ROOT/deploy/gateway" \
	--namespace agentgateway-system >"$state/gateway.yaml"

kubectl kustomize "$REPO_ROOT/deploy/pagerless-server" >"$state/pagerless-server.yaml"
kubectl kustomize "$REPO_ROOT/deploy/pagerless-executor" >"$state/pagerless-executor.yaml"
kubectl kustomize "$REPO_ROOT/deploy/observability/dashboards" >"$state/dashboards.yaml"

kubeconform -strict -ignore-missing-schemas -summary \
	"$state/root.yaml" \
	"$state/apps.yaml" \
	"$state/mcp.yaml" \
	"$state/keycloak.yaml" \
	"$state/gateway.yaml" \
	"$state/pagerless-server.yaml" \
	"$state/pagerless-executor.yaml" \
	"$state/dashboards.yaml" \
	"$REPO_ROOT"/deploy/observability/alerts/*.yaml \
	"$REPO_ROOT/deploy/observability/otlp-receiver/prometheus-service.yaml" \
	"$REPO_ROOT/evals/e2e/manifests/adapter.yaml"

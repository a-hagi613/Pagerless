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

# The chart ships its own copy of the generated CRDs so that a standalone
# installation does not depend on the kustomize bases. make manifests writes
# both copies, and drift between them fails here.
for crd in pagerless.dev_triages.yaml pagerless.dev_triageapprovals.yaml; do
	cmp -s "$REPO_ROOT/deploy/pagerless-server/crds/$crd" "$REPO_ROOT/charts/pagerless/crds/$crd" ||
		die "charts/pagerless/crds/$crd differs from deploy/pagerless-server/crds/$crd; run make manifests."
done

helm lint "$REPO_ROOT/charts/pagerless"

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

# The Pagerless chart in its default live-only mode, in GitOps mode with the
# placeholder Secrets that a standalone trial uses, and through the installer's
# own dry run, which is the path hack/install.sh actually takes.
helm template pagerless "$REPO_ROOT/charts/pagerless" \
	--namespace pagerless >"$state/chart-live.yaml"

helm template pagerless "$REPO_ROOT/charts/pagerless" \
	--namespace pagerless \
	--set secrets.createPlaceholders=true \
	--set "workloadNamespaces={payments,checkout}" \
	--set-string "sandbox.url=https://github.com/example/workloads" \
	--set-string "argocd.application=workloads" >"$state/chart-gitops.yaml"

bash "$REPO_ROOT/hack/install.sh" --dry-run --context validate-ctx \
	--namespaces payments,checkout --provider anthropic >"$state/install-dry-run.yaml"

kubeconform -strict -ignore-missing-schemas -summary \
	"$state/root.yaml" \
	"$state/apps.yaml" \
	"$state/mcp.yaml" \
	"$state/keycloak.yaml" \
	"$state/gateway.yaml" \
	"$state/pagerless-server.yaml" \
	"$state/pagerless-executor.yaml" \
	"$state/dashboards.yaml" \
	"$state/chart-live.yaml" \
	"$state/chart-gitops.yaml" \
	"$state/install-dry-run.yaml" \
	"$REPO_ROOT"/deploy/observability/alerts/*.yaml \
	"$REPO_ROOT/deploy/observability/otlp-receiver/prometheus-service.yaml" \
	"$REPO_ROOT/evals/e2e/manifests/adapter.yaml"

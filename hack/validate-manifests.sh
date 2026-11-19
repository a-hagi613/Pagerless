#!/usr/bin/env bash
# Renders the Pagerless chart and validates the result with the pinned
# kubeconform. Custom resources have no schema in kubeconform's default
# catalog, so they are skipped; every core Kubernetes object the chart renders
# is checked.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands helm kubeconform
case "$(helm version --short 2>/dev/null)" in
v"${HELM_VERSION}"* | "${HELM_VERSION}"*) ;;
*) die "helm v${HELM_VERSION} is required to validate the manifests." ;;
esac
[ "$(kubeconform -v 2>/dev/null)" = "$KUBECONFORM_VERSION" ] ||
	die "kubeconform $KUBECONFORM_VERSION is required to validate the manifests."

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-manifests.XXXXXXXX")"
chmod 700 "$state"
trap 'rm -rf "$state"' EXIT

helm lint "$REPO_ROOT/charts/pagerless"

# The chart in its default live-only mode, in GitOps mode with the placeholder
# Secrets that a standalone trial uses, and through the installer's own dry run,
# which is the path hack/install.sh actually takes.
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
	"$state/chart-live.yaml" \
	"$state/chart-gitops.yaml" \
	"$state/install-dry-run.yaml"

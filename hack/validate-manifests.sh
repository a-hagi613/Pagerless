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

# The chart in both watch modes, with an externally managed credentials Secret,
# and through the installer's own dry run, which is the path hack/install.sh
# actually takes.
helm template pagerless "$REPO_ROOT/charts/pagerless" \
	--namespace pagerless --include-crds >"$state/chart-cluster-wide.yaml"

helm template pagerless "$REPO_ROOT/charts/pagerless" \
	--namespace pagerless --include-crds \
	--set rbac.watchAllNamespaces=false \
	--set "rbac.watchNamespaces={payments,checkout}" \
	--set credentials.existingSecret=team-credentials >"$state/chart-allowlist.yaml"

bash "$REPO_ROOT/hack/install.sh" --dry-run --namespaces payments,checkout >"$state/install-dry-run.yaml"

kubeconform -strict -ignore-missing-schemas -summary \
	"$state/chart-cluster-wide.yaml" \
	"$state/chart-allowlist.yaml" \
	"$state/install-dry-run.yaml"

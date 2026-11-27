#!/usr/bin/env bash
# Renders the packaged chart and validates every object with the pinned
# kubeconform. The chart has no custom resources; a missing schema is a
# failure, not something to skip.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands helm kubeconform jq
case "$(helm version --short 2>/dev/null)" in
v"${HELM_VERSION}"* | "${HELM_VERSION}"*) ;;
*) die "helm ${HELM_VERSION} is required to validate the manifests." ;;
esac
[ "$(kubeconform -v 2>/dev/null)" = "$KUBECONFORM_VERSION" ] ||
	die "kubeconform ${KUBECONFORM_VERSION} is required to validate the manifests."

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-manifests.XXXXXXXX")"
chmod 700 "$state"
trap 'rm -rf "$state"' EXIT

jq empty "$REPO_ROOT/charts/pagerless/values.schema.json"

required=(
	--set existingSecret=pagerless-credentials
	--set model.baseUrl=https://model.example/v1
	--set model.name=tool-model
	--set github.repository=acme/cluster
)

helm lint "$REPO_ROOT/charts/pagerless" "${required[@]}"

# A missing required setting must fail with the setting's name, so an installer
# learns what to supply rather than installing a broken release.
if helm template pagerless "$REPO_ROOT/charts/pagerless" \
	--set existingSecret=pagerless-credentials \
	--set model.baseUrl=https://model.example/v1 \
	--set model.name=tool-model >"$state/missing.log" 2>&1; then
	die "the chart rendered without a GitHub repository."
fi
grep -q "github.repository" "$state/missing.log" ||
	die "the missing-value error does not name github.repository."

helm template pagerless "$REPO_ROOT/charts/pagerless" --namespace pagerless \
	"${required[@]}" >"$state/cluster-wide.yaml"
helm template pagerless "$REPO_ROOT/charts/pagerless" --namespace pagerless \
	"${required[@]}" \
	--set persistence.existingClaim=operator-transcript \
	--set "kubernetes.namespaceAllowlist={shop,payments}" >"$state/restricted.yaml"
helm template pagerless "$REPO_ROOT/charts/pagerless" --namespace pagerless \
	"${required[@]}" --set persistence.enabled=false >"$state/no-persistence.yaml"

for manifest in "$state"/*.yaml; do
	kubeconform -strict -summary -kubernetes-version 1.37.0 "$manifest"
done

# The chart references an externally provisioned Secret and never creates one.
if grep -q '^kind: Secret$' "$state"/*.yaml; then
	die "the chart rendered a Secret; credentials are externally provisioned."
fi

# The installation is private by default.
grep -q 'type: ClusterIP' "$state/cluster-wide.yaml" ||
	die "the server Service is not private."
if grep -q 'type: LoadBalancer\|type: NodePort\|kind: Ingress' "$state"/*.yaml; then
	die "the chart exposes an external listener."
fi

# The cluster identity reads and cannot mutate or read Secrets.
if grep -q 'resources:\s*$' "$state/cluster-wide.yaml" &&
	grep -q '^ *- secrets$' "$state/cluster-wide.yaml"; then
	die "the rendered RBAC grants Secrets."
fi
if grep -qE '^\s*- (create|update|patch|delete|deletecollection)$' "$state/cluster-wide.yaml"; then
	die "the rendered RBAC grants a mutating verb."
fi
grep -q 'resources:' "$state/cluster-wide.yaml" ||
	die "the rendered chart grants no read rules."

# The transcript has one writer.
grep -q 'replicas: 1' "$state/cluster-wide.yaml" ||
	die "the server does not run as exactly one replica."

# NOTES must print the actual service and the port-forward command.
helm install pagerless "$REPO_ROOT/charts/pagerless" --namespace pagerless \
	--dry-run=client "${required[@]}" >"$state/install.txt"
grep -q 'port-forward service/pagerless-server' "$state/install.txt" ||
	die "the install notes do not print the port-forward command."

echo "manifests: every rendered object is valid and the access boundary holds."

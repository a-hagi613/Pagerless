#!/usr/bin/env bash
# Builds the Pagerless images, loads them into a local kind cluster, provisions a
# development-only credentials Secret, and installs the chart from the checkout.
#
# This is a developer loop, not a release path. The chart itself never creates a
# Secret; this script creates one because a local cluster has no external secret
# manager.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands docker kind kubectl helm go
bash "$REPO_ROOT/hack/preflight.sh"

required_env=(PAGERLESS_MODEL_API_KEY PAGERLESS_GITHUB_READ_TOKEN PAGERLESS_GITHUB_WRITE_TOKEN)
for name in "${required_env[@]}"; do
	if [ -z "${!name:-}" ]; then
		die "${name} is required for make dev. Export it in your shell; it is never written to the repository."
	fi
done
[ -n "${PAGERLESS_MODEL_BASE_URL:-}" ] || die "PAGERLESS_MODEL_BASE_URL is required (an OpenAI-compatible endpoint)."
[ -n "${PAGERLESS_MODEL_NAME:-}" ] || die "PAGERLESS_MODEL_NAME is required (a model that supports tool calls)."
[ -n "${PAGERLESS_GITHUB_REPOSITORY:-}" ] || die "PAGERLESS_GITHUB_REPOSITORY is required (owner/name)."

if ! kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --wait 120s
fi

make -C "$REPO_ROOT" build-images
kind load docker-image --name "$KIND_CLUSTER_NAME" \
	"$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_AGENT_IMAGE"

operator_token="${PAGERLESS_OPERATOR_TOKEN:-$(openssl rand -hex 32)}"
internal_token="${PAGERLESS_INTERNAL_TOKEN:-$(openssl rand -hex 32)}"
mcp_token="${PAGERLESS_MCP_TOKEN:-$(openssl rand -hex 32)}"

kubectl --context "kind-${KIND_CLUSTER_NAME}" create namespace pagerless --dry-run=client -o yaml | kubectl --context "kind-${KIND_CLUSTER_NAME}" apply -f -
kubectl --context "kind-${KIND_CLUSTER_NAME}" -n pagerless create secret generic pagerless-credentials \
	--from-literal=operator-token="$operator_token" \
	--from-literal=internal-token="$internal_token" \
	--from-literal=mcp-token="$mcp_token" \
	--from-literal=github-read-token="$PAGERLESS_GITHUB_READ_TOKEN" \
	--from-literal=github-write-token="$PAGERLESS_GITHUB_WRITE_TOKEN" \
	--from-literal=model-api-key="$PAGERLESS_MODEL_API_KEY" \
	--dry-run=client -o yaml | kubectl --context "kind-${KIND_CLUSTER_NAME}" apply -f -

bash "$REPO_ROOT/hack/install.sh" \
	--context "kind-${KIND_CLUSTER_NAME}" \
	--secret pagerless-credentials \
	--model-base-url "$PAGERLESS_MODEL_BASE_URL" \
	--model-name "$PAGERLESS_MODEL_NAME" \
	--github-repository "$PAGERLESS_GITHUB_REPOSITORY" \
	--github-base-branch "${PAGERLESS_GITHUB_BASE_BRANCH:-main}" \
	--github-path-prefix "${PAGERLESS_GITHUB_PATH_PREFIX:-}" \
	--set "image.tag=${PAGERLESS_SERVER_IMAGE##*:}" \
	--set "agentImage.tag=${PAGERLESS_AGENT_IMAGE##*:}"

echo
echo "The development operator token is in Secret pagerless/pagerless-credentials (key operator-token)."
echo "Nothing was written to the repository."

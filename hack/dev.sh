#!/usr/bin/env bash
# Builds the Pagerless runtime images, loads them into a local kind cluster, and
# installs the chart from the checkout. The chart shell serves the agent report
# API and the metrics API; the Portal arrives with its own slice.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands docker kind kubectl helm
bash "$REPO_ROOT/hack/preflight.sh"

if ! kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	kind create cluster --name "$KIND_CLUSTER_NAME" --image "$KIND_NODE_IMAGE" --wait 120s
fi

make -C "$REPO_ROOT" build-images
kind load docker-image --name "$KIND_CLUSTER_NAME" \
	"$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_AGENT_IMAGE"

bash "$REPO_ROOT/hack/install.sh" --context "kind-${KIND_CLUSTER_NAME}" \
	--namespaces "${WORKLOAD_NAMESPACES:-default}" --yes

echo
echo "Pagerless is installed in kind cluster '$KIND_CLUSTER_NAME'."
echo "Reach the agent report API and metrics with:"
echo "  kubectl --context kind-${KIND_CLUSTER_NAME} -n pagerless port-forward svc/pagerless-server 8080:8080"

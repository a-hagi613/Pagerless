#!/usr/bin/env bash
# Deletes the local development cluster that make dev created.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

if kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then
	kind delete cluster --name "$KIND_CLUSTER_NAME"
else
	echo "No kind cluster named $KIND_CLUSTER_NAME exists."
fi

echo "Deleted the local development cluster."

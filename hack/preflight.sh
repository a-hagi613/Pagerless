#!/usr/bin/env bash
# Checks the prerequisites of make dev and reports every unmet one before any
# cluster change is made.
set -uo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

unmet=0
fail() {
	echo "preflight: $*" >&2
	unmet=$((unmet + 1))
}

if ! command -v docker >/dev/null 2>&1; then
	fail "docker is not installed. Install Docker and enable its integration with your shell."
elif ! docker info >/dev/null 2>&1; then
	fail "the Docker engine is not reachable. Start Docker and try again."
else
	mem_bytes=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
	mem_gib=$((mem_bytes / 1024 / 1024 / 1024))
	if [ "$mem_gib" -lt "$MIN_MEMORY_GIB" ]; then
		fail "the Docker engine has ${mem_gib} GiB of memory; at least ${MIN_MEMORY_GIB} GiB is required. Raise the engine's memory limit."
	fi
fi

if ! command -v kind >/dev/null 2>&1; then
	fail "kind is not installed. Install kind ${KIND_VERSION}."
elif ! kind version 2>/dev/null | grep -q "^kind ${KIND_VERSION} "; then
	fail "kind ${KIND_VERSION} is required; found: $(kind version 2>/dev/null)."
fi

if ! command -v kubectl >/dev/null 2>&1; then
	fail "kubectl is not installed. Install kubectl ${KUBECTL_VERSION}."
elif ! kubectl version --client 2>/dev/null | grep -qx "Client Version: ${KUBECTL_VERSION}"; then
	fail "kubectl ${KUBECTL_VERSION} is required; found: $(kubectl version --client 2>/dev/null | head -n 1)."
fi

if ! command -v helm >/dev/null 2>&1; then
	fail "helm is not installed. Install Helm ${HELM_VERSION}."
elif ! helm version --short 2>/dev/null | grep -q "^${HELM_VERSION}+"; then
	fail "Helm ${HELM_VERSION} is required; found: $(helm version --short 2>/dev/null)."
fi

if [ "$unmet" -gt 0 ]; then
	die "$unmet preflight requirement(s) unmet. Fix the items above and run make dev again."
fi
echo "preflight: all requirements met"

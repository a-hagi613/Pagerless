# shellcheck shell=bash
# Shared helpers for the scripts in hack/. Sourced by each script, never executed.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

die() {
	echo "error: $*" >&2
	exit 1
}

# Loads the pinned versions from hack/versions.env into the environment.
load_versions() {
	set -a
	. "$REPO_ROOT/hack/versions.env"
	set +a
}

require_commands() {
	local command_name
	for command_name in "$@"; do
		command -v "$command_name" >/dev/null 2>&1 ||
			die "$command_name is required for this command but was not found on PATH."
	done
}

# Runs kubectl against the pagerless development cluster, never the current context.
kc() {
	kubectl --context "kind-${KIND_CLUSTER_NAME}" "$@"
}

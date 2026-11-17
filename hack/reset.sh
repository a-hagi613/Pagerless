#!/usr/bin/env bash
# Force-resets the Sandbox's main branch to the known-good tag, then waits for
# Argo CD to sync that commit and for the Workload to become healthy again.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions
load_env

require_env SANDBOX_URL GITHUB_WRITE_TOKEN
require_commands git
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Scenario 9 is applied outside Git, so reset removes its experiment before it
# restores the Sandbox branch.
bash "$REPO_ROOT/hack/cpu-stress.sh" clear

git_authenticated clone --quiet "$SANDBOX_URL" "$work/sandbox"
cd "$work/sandbox"
git_authenticated fetch --quiet origin "refs/tags/known-good:refs/tags/known-good" ||
	die "the Sandbox has no known-good tag."
known_good="$(git rev-parse --verify "refs/tags/known-good^{commit}")"

git_authenticated push --quiet --force origin "$known_good:refs/heads/main"
echo "Reset the Sandbox main branch to known-good ($known_good). Waiting for Argo CD to sync it."

cd "$REPO_ROOT"
wait_for_application sandbox "$known_good" 300 healthy
kc -n sandbox rollout status deployment/checkout --timeout=180s

echo "The Sandbox is at known-good and the checkout Workload is healthy."

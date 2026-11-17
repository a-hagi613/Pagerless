#!/usr/bin/env bash
# Runs the evaluation against the running local kind stack. The stack's agent
# provider decides whether the run is live (a paid model) or Replay; a Replay
# run with only the committed Phase 1 recording is refused, and the
# credential-free subset is `make eval-replay`.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions
load_env

require_commands kubectl make openssl git uv
[ "$(uv --version | cut -d ' ' -f 2)" = "$UV_VERSION" ] ||
	die "uv $UV_VERSION is required for the evaluation harness."
# Rebuild the CLI so that a manual Trigger uses this checkout's code.
make -C "$REPO_ROOT" build-cli

exec uv run --no-project --python "$PYTHON_VERSION" python "$REPO_ROOT/evals/harness/eval.py" "$@"

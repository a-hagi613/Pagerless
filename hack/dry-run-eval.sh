#!/usr/bin/env bash
# Offline dry run of the full paid evaluation in well under a minute: no cluster,
# no model call, no spend. It checks the files a batch depends on, then runs the
# twenty single-run shards from fixtures and merges them the way `aggregate` does.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands go grep uv

cd "$REPO_ROOT"

echo "[1/3] Checking the Scenario injection and the flag Scenarios it asserts..."
bash -n hack/break.sh
grep -q '"defaultVariant": "100x"' scenarios/email-memory-leak/inject.patch ||
	die "the email-memory-leak Scenario does not commit the 100x variant."
grep -q 'wait_for_flag emailMemoryLeak "100x"' hack/break.sh ||
	die "hack/break.sh does not wait for the emailMemoryLeak 100x variant."

echo "[2/3] Running the sandbox and alert rule tests..."
go test ./internal/sandbox/... ./internal/alerts/...

echo "[3/3] Building the twenty shards from fixtures and merging them..."
cd "$REPO_ROOT/evals/harness"
uv run --no-project --python "$PYTHON_VERSION" python dry_run.py

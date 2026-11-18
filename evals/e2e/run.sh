#!/usr/bin/env bash
# Runs the adapter-backed flow in a dedicated local kind cluster.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
. "$repo_root/hack/versions.env"
python=(uv run --no-project --python "$PYTHON_VERSION" python)
python_executable="$("${python[@]}" -c 'import sys; print(sys.executable)')"
python=("$python_executable")
PYTHONPATH="$script_dir" "${python[@]}" -m unittest discover -s "$script_dir" -p 'test_*.py'

keep="${PAGERLESS_E2E_KEEP_CLUSTER:-0}"
case "$keep" in 0|1) ;; *) echo "PAGERLESS_E2E_KEEP_CLUSTER must be 0 or 1." >&2; exit 1 ;; esac
# The harness plays one Scenario. Each Scenario's Replay recording lives beside
# this script, named for it, so a Scenario is selected without another flag.
scenario="${PAGERLESS_E2E_SCENARIO:-oom}"
provider="${PAGERLESS_E2E_MODEL_PROVIDER:-replay}"
recording="${PAGERLESS_E2E_RECORDING:-synthetic-$scenario.json}"
# capture asks the stack to write the genuine recordings, which the agent refuses
# in Replay, so it is a separate switch from PAGERLESS_E2E_RECORDING, the file
# that Replay plays.
capture="${PAGERLESS_E2E_CAPTURE:-0}"
case "$capture" in 0|1) ;; *) echo "PAGERLESS_E2E_CAPTURE must be 0 or 1." >&2; exit 1 ;; esac
if [ "$capture" = 1 ] && [ "$provider" != anthropic ]; then
	echo "PAGERLESS_E2E_CAPTURE requires PAGERLESS_E2E_MODEL_PROVIDER=anthropic." >&2
	exit 1
fi
case "$scenario" in *[!a-z0-9-]*|"") echo "PAGERLESS_E2E_SCENARIO must be a lowercase Scenario name." >&2; exit 1 ;; esac
# Only Replay reads a recording, so only Replay needs the fixture to exist.
if [ "$provider" = replay ]; then
	[ -f "$script_dir/fixtures/$recording" ] ||
		{ echo "The Replay recording $recording is not in evals/e2e/fixtures." >&2; exit 1; }
fi
if kind get clusters | grep -qx "$KIND_CLUSTER_NAME" && [ "${PAGERLESS_E2E_REUSE_CLUSTER:-0}" != 1 ]; then
	echo "The kind cluster already exists; set PAGERLESS_E2E_REUSE_CLUSTER=1 only for the isolated E2E cluster." >&2
	exit 1
fi
"${python[@]}" -c 'import socket; s=[socket.socket() for _ in range(2)]; [x.bind(("127.0.0.1", p)) for x,p in zip(s,(18443,18080))]; [x.close() for x in s]'
state_is_temporary=0
if [ -n "${PAGERLESS_E2E_STATE_DIR:-}" ]; then
	state="$PAGERLESS_E2E_STATE_DIR"
	[ ! -e "$state" ] || { echo "PAGERLESS_E2E_STATE_DIR must not already exist." >&2; exit 1; }
else
	state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-e2e.XXXXXXXX")"
	state_is_temporary=1
fi
mkdir -p "$state/certs" "$state/bin"
chmod 700 "$state" "$state/certs" "$state/bin"
created=0
forward=""
observer=""
policies_saved=0
. "$script_dir/cleanup.sh"
. "$script_dir/diagnostics.sh"
PAGERLESS_E2E_DIAGNOSTICS="${PAGERLESS_E2E_DIAGNOSTICS:-$state/diagnostics}"
export PAGERLESS_E2E_DIAGNOSTICS
trap 'result=$?; trap - EXIT; if [ "$result" != 0 ]; then dump_e2e_diagnostics; fi; cleanup_e2e "$result"; exit "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go run "$script_dir/certgen" "$state/certs"
# The GitHub MCP server accepts only GitHub-shaped bearer tokens, so the
# disposable read token carries a GitHub prefix and an unmistakable e2e marker.
read_token="$("${python[@]}" -c 'import secrets; print("ghp_e2e" + secrets.token_hex(16))')"
write_token="$("${python[@]}" -c 'import secrets; print("e2e-write-" + secrets.token_urlsafe(32))')"
archive="$state/kubeconform.tar.gz"
curl --fail --silent --show-error --location --output "$archive" https://github.com/yannh/kubeconform/releases/download/v0.8.0/kubeconform-linux-amd64.tar.gz
printf '%s  %s\n' 9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 "$archive" | sha256sum --check --status || { echo "kubeconform v0.8.0 checksum mismatch." >&2; exit 1; }
tar -xzf "$archive" -C "$state/bin" kubeconform
chmod 700 "$state/bin/kubeconform"
umask 077
cat >"$state/config.env" <<EOF
SANDBOX_URL=https://sandbox-adapter.pagerless-e2e.svc:8443/git/pagerless-test/sandbox.git
GITHUB_READ_TOKEN=$read_token
GITHUB_WRITE_TOKEN=$write_token
MODEL_PROVIDER=$provider
PAGERLESS_REPLAY_FILE=/app/recordings/$recording
PAGERLESS_SANDBOX_OWNER=pagerless-test
PAGERLESS_SANDBOX_REPO=sandbox
PAGERLESS_GITHUB_REPOSITORY=pagerless-test/sandbox
PAGERLESS_MCP_GITHUB_HOST=http://127.0.0.1:8083
PAGERLESS_E2E=1
PAGERLESS_E2E_TOKEN=$write_token
PAGERLESS_E2E_REPOSITORY=pagerless-test/sandbox
PAGERLESS_E2E_GIT_HOST=sandbox-adapter.pagerless-e2e.svc
PAGERLESS_E2E_API_URL=https://localhost:18443
PAGERLESS_E2E_CONTROL_URL=http://127.0.0.1:18080
PAGERLESS_E2E_GIT_URL=https://localhost:18443/git/pagerless-test/sandbox.git
EOF
printf 'PAGERLESS_E2E_CA_FILE=%q\n' "$state/certs/ca.crt" >>"$state/config.env"
printf 'PAGERLESS_E2E_TLS_CERT_FILE=%q\n' "$state/certs/tls.crt" >>"$state/config.env"
printf 'PAGERLESS_E2E_TLS_KEY_FILE=%q\n' "$state/certs/tls.key" >>"$state/config.env"
printf 'PAGERLESS_E2E_KUBECONFORM_BIN=%q\n' "$state/bin/kubeconform" >>"$state/config.env"
if [ "$provider" = anthropic ]; then
	[ -n "${ANTHROPIC_API_KEY:-}" ] || { echo "ANTHROPIC_API_KEY is required for a live run." >&2; exit 2; }
	printf 'ANTHROPIC_API_KEY=%q\n' "$ANTHROPIC_API_KEY" >>"$state/config.env"
fi
unset read_token write_token
# The server, Executor, and relay read the adapter CA and tokens when they
# start. Roll their pod templates whenever this run's material changes so a
# reused cluster never keeps credentials from the previous run.
config_checksum="$(cat "$state/config.env" "$state/certs/ca.crt" | sha256sum | cut -d ' ' -f 1)"
if ! kind get clusters | grep -qx "$KIND_CLUSTER_NAME"; then created=1; fi
export PAGERLESS_ENV_FILE="$state/config.env" PAGERLESS_E2E=1
export PLATFORM_REPO_URL=https://github.com/a-hagi613/pagerless
if [ -z "${PLATFORM_REVISION:-}" ]; then
	PLATFORM_REVISION="${GITHUB_SHA:-}"
fi
[[ "$PLATFORM_REVISION" =~ ^[0-9a-f]{40}$ ]] || {
	echo "Set PLATFORM_REVISION to the exact pushed Pagerless commit before running the local E2E harness." >&2
	exit 1
}
export PLATFORM_REVISION
export PAGERLESS_BUILD_REVISION="$PLATFORM_REVISION"
make -C "$repo_root" dev

# A run that sets PAGERLESS_E2E_MEMORY_PEAK samples the memory that the whole
# stack uses, so a published requirement can rest on a measurement instead of an
# estimate. The samples are kept verbatim in that file for the peak to be read
# from, and the node container is the one that holds every pod in the cluster.
if [ -n "${PAGERLESS_E2E_MEMORY_PEAK:-}" ]; then
	(
		while :; do
			docker stats --no-stream --format '{{.MemUsage}}' "${KIND_CLUSTER_NAME}-control-plane" 2>/dev/null || true
			sleep 5
		done
	) >"$PAGERLESS_E2E_MEMORY_PEAK" &
	sampler=$!
fi

# The root Application owns the child Application specs. Pause reconciliation
# before applying isolated TLS and relay patches; keep the Sandbox Application active.
for app in root pagerless-server pagerless-executor mcp; do
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n argocd get application "$app" -o json |
		"${python[@]}" -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["spec"]["syncPolicy"]))' >"$state/$app-sync-policy.json"
done
policies_saved=1
kubectl --context "kind-$KIND_CLUSTER_NAME" -n argocd patch application root \
	--type=json --patch='[{"op":"remove","path":"/spec/syncPolicy"}]'
for app in pagerless-server pagerless-executor mcp; do
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n argocd patch application "$app" \
		--type=json --patch='[{"op":"remove","path":"/spec/syncPolicy"}]'
done
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment pagerless-server --type strategic --patch-file "$script_dir/patches/pagerless-server.yaml"
# ADR-0006 keeps the five-minute observation window for production, for every
# live run, and for the single-Scenario check. The credential-free Replay
# evaluation checks the pipeline rather than recovery confidence, so it shortens
# the window and tells the harness which window the recorded evidence must show.
verification_window=300
if [ "$provider" = replay ] && [ "${PAGERLESS_E2E_EVAL:-0}" = "1" ]; then
	verification_window=60
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment pagerless-server --type strategic \
		--patch '{"spec":{"template":{"spec":{"containers":[{"name":"server","env":[{"name":"PAGERLESS_VERIFICATION_WINDOW","value":"60s"}]}]}}}}'
fi
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment pagerless-executor --type strategic --patch-file "$script_dir/patches/pagerless-executor.yaml"
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment mcp-github --type strategic --patch-file "$script_dir/patches/github-mcp-relay.yaml"
for deployment in pagerless-server pagerless-executor mcp-github; do
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment "$deployment" --type=merge \
		--patch "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"pagerless.dev/config-checksum\":\"$config_checksum\"}}}}}"
done
# Only Replay reads the configured recording; a live run leaves it untouched.
if [ "$provider" = replay ]; then
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch configmap pagerless-agent-config --type merge --patch "{\"data\":{\"PAGERLESS_REPLAY_FILE\":\"/app/recordings/$recording\"}}"
fi
for deployment in pagerless-server pagerless-executor mcp-github; do kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless rollout status "deployment/$deployment" --timeout=180s; done

# A live evaluation can also produce the genuine Replay recordings. The
# destination claim and the inference Secret are provisioned with the same
# stdin-only path as a manual capture, then the server is restarted so the
# investigation and Narrative Jobs record to the claim.
if [ "$capture" = 1 ]; then
	(
		cd "$repo_root/agent" &&
			uv run --locked python "$repo_root/evals/replay/capture.py" prepare \
				--context "kind-$KIND_CLUSTER_NAME" --key-file "$state/config.env" --image "$PAGERLESS_AGENT_IMAGE"
	)
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch configmap pagerless-agent-config --type merge \
		--patch '{"data":{"PAGERLESS_MODEL_PROVIDER":"anthropic","PAGERLESS_RECORDING_PVC":"pagerless-replay-capture","PAGERLESS_RECORDING_FILE":"/recordings/oom.json","PAGERLESS_NARRATIVE_RECORDING_FILE":"/recordings/narrative.json"}}'
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless rollout restart deployment/pagerless-server
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless rollout status deployment/pagerless-server --timeout=180s
fi
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless-e2e port-forward deployment/sandbox-adapter 18443:8443 18080:8080 >"$state/port-forward.log" 2>&1 &
forward=$!
attempt=0
while [ "$attempt" -lt 60 ]; do
	if curl --fail --silent --cacert "$state/certs/ca.crt" https://localhost:18443/healthz >/dev/null && curl --fail --silent http://127.0.0.1:18080/healthz >/dev/null; then break; fi
	sleep 1
	attempt=$((attempt + 1))
done
curl --fail --silent --cacert "$state/certs/ca.crt" https://localhost:18443/healthz >/dev/null
set -a
. "$state/config.env"
set +a
# The evaluation subset reuses this isolated cluster and adapter for several
# Scenarios. The harness resets, injects, approves as eval-approver, and scores
# each run; this runner still owns the cluster, the adapter, and its cleanup.
if [ "${PAGERLESS_E2E_EVAL:-0}" = "1" ]; then
	. "$repo_root/hack/lib.sh"
	load_versions
	export GIT_AUTHOR_NAME="Pagerless E2E" GIT_AUTHOR_EMAIL=e2e@pagerless.dev
	export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
	make -C "$repo_root" build-cli
	eval_arguments=(--adapter --scenarios "${PAGERLESS_E2E_SCENARIOS:-oom}"
		--repeats "${PAGERLESS_E2E_REPEATS:-1}"
		--verification-window "$verification_window"
		--results-dir "$repo_root/evals/results")
	if [ -n "${PAGERLESS_E2E_MAX_TOKENS:-}" ]; then
		eval_arguments+=(--max-tokens "$PAGERLESS_E2E_MAX_TOKENS")
	fi
	if [ -n "${PAGERLESS_E2E_MAX_COST_USD:-}" ]; then
		eval_arguments+=(--max-cost-usd "$PAGERLESS_E2E_MAX_COST_USD")
	fi
	if [ -n "${PAGERLESS_E2E_REPEAT_NUMBER:-}" ]; then
		eval_arguments+=(--repeat-number "$PAGERLESS_E2E_REPEAT_NUMBER")
	fi
	if [ "${PAGERLESS_E2E_REQUIRE_PASS:-1}" = "1" ]; then
		eval_arguments+=(--require-pass)
	fi
	"${python[@]}" "$repo_root/evals/harness/eval.py" "${eval_arguments[@]}"
	if [ "$capture" = 1 ]; then
		triage="$(kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless \
			get triages --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')"
		[ -n "$triage" ] || { echo "the recorded evaluation produced no Triage" >&2; exit 1; }
		# The Narrative Job starts after Verification; wait for its text before
		# retrieving the recording.
		for _ in $(seq 1 36); do
			narrative="$(kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless \
				get triage "$triage" -o jsonpath='{.status.narrative}')"
			[ -n "$narrative" ] && break
			sleep 5
		done
		(
			cd "$repo_root/agent"
			uv run --locked python "$repo_root/evals/replay/capture.py" retrieve \
				--context "kind-$KIND_CLUSTER_NAME" --key-file "$state/config.env" \
				--image "$PAGERLESS_AGENT_IMAGE" --artifact "$repo_root/evals/replay/oom.json"
			uv run --locked python "$repo_root/evals/replay/capture.py" provenance \
				--context "kind-$KIND_CLUSTER_NAME" --key-file "$state/config.env" \
				--image "$PAGERLESS_AGENT_IMAGE" --triage "$triage" \
				--source-revision "$PLATFORM_REVISION" --artifact "$repo_root/evals/replay/oom.json"
			uv run --locked python "$repo_root/evals/replay/capture.py" retrieve-narrative \
				--context "kind-$KIND_CLUSTER_NAME" --key-file "$state/config.env" \
				--image "$PAGERLESS_AGENT_IMAGE" --narrative-artifact "$repo_root/evals/replay/narrative.json"
		)
	fi
	uv run --no-project --python "$PYTHON_VERSION" "$script_dir/client.py" \
		--repository "$PAGERLESS_E2E_REPOSITORY" reset
	wait_for_application sandbox 9eff68bdf72bcf9e99d6d713a152995d851aa841 900 healthy
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless delete triages --all --wait=true
	if [ "$keep" = 1 ]; then
		echo "Retained the reset healthy kind cluster and restricted state in $state."
	else
		echo "Credential-free Replay evaluation passed."
	fi
	exit 0
fi
uv run --no-project --python "$PYTHON_VERSION" "$script_dir/client.py" \
	--repository "$PAGERLESS_E2E_REPOSITORY" reset
. "$repo_root/hack/lib.sh"
load_versions
wait_for_application sandbox 9eff68bdf72bcf9e99d6d713a152995d851aa841 900 healthy
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless delete triages --all --wait=true
history="$state/phase-history.txt"
snapshot="$state/triage.json"
"${python[@]}" "$script_dir/triage.py" collect --history "$history" --snapshot "$snapshot" --timeout 1200 & observer=$!
export GIT_AUTHOR_NAME="Pagerless E2E" GIT_AUTHOR_EMAIL=e2e@pagerless.dev
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
make -C "$repo_root" break SCENARIO="$scenario"
triage="$("${python[@]}" "$script_dir/triage.py" proposed --timeout 300)"
make -C "$repo_root" build-cli
PATH="$repo_root/bin:$PATH" kubectl triage approve "$triage" --yes
uv run --no-project --python "$PYTHON_VERSION" "$script_dir/checks.py" --triage "$triage" --timeout 300
wait "$observer"
observer=""
"${python[@]}" "$script_dir/triage.py" verify --history "$history" --snapshot "$snapshot"
set -a
. "$state/config.env"
set +a
uv run --no-project --python "$PYTHON_VERSION" "$script_dir/client.py" --repository "$PAGERLESS_E2E_REPOSITORY" reset
wait_for_application sandbox 9eff68bdf72bcf9e99d6d713a152995d851aa841 900 healthy
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless delete triages --all --wait=true
if [ "$keep" = 1 ]; then
	echo "Retained the reset healthy kind cluster and restricted state in $state."
else
	echo "Credential-free adapter integration passed."
fi

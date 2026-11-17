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
trap 'result=$?; trap - EXIT; cleanup_e2e "$result"; exit "$?"' EXIT
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
MODEL_PROVIDER=replay
PAGERLESS_REPLAY_FILE=/app/recordings/synthetic-oom.json
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
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment pagerless-executor --type strategic --patch-file "$script_dir/patches/pagerless-executor.yaml"
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment mcp-github --type strategic --patch-file "$script_dir/patches/github-mcp-relay.yaml"
for deployment in pagerless-server pagerless-executor mcp-github; do
	kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch deployment "$deployment" --type=merge \
		--patch "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"pagerless.dev/config-checksum\":\"$config_checksum\"}}}}}"
done
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless patch configmap pagerless-agent-config --type merge --patch '{"data":{"PAGERLESS_REPLAY_FILE":"/app/recordings/synthetic-oom.json"}}'
for deployment in pagerless-server pagerless-executor mcp-github; do kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless rollout status "deployment/$deployment" --timeout=180s; done
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
uv run --no-project --python "$PYTHON_VERSION" "$script_dir/client.py" \
	--repository "$PAGERLESS_E2E_REPOSITORY" reset
. "$repo_root/hack/lib.sh"
load_versions
wait_for_application sandbox 85f60904a8eb245e0e337e4c463863cbd59404e9 300 healthy
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless delete triages --all --wait=true
history="$state/phase-history.txt"
snapshot="$state/triage.json"
"${python[@]}" "$script_dir/triage.py" collect --history "$history" --snapshot "$snapshot" --timeout 1200 & observer=$!
export GIT_AUTHOR_NAME="Pagerless E2E" GIT_AUTHOR_EMAIL=e2e@pagerless.dev
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME" GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
make -C "$repo_root" break SCENARIO=oom
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
wait_for_application sandbox 85f60904a8eb245e0e337e4c463863cbd59404e9 300 healthy
kubectl --context "kind-$KIND_CLUSTER_NAME" -n pagerless delete triages --all --wait=true
if [ "$keep" = 1 ]; then
	echo "Retained the reset healthy kind cluster and restricted state in $state."
else
	echo "Credential-free adapter integration passed."
fi

#!/usr/bin/env bash
# Runs the dashboard smoke test: a Keycloak container for the real sign-in flow,
# the envtest API server and dashboard (with a stand-in agent), and Chromium
# driven by Playwright. Nothing is printed that could contain a credential.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
dashboard_dir="$repo_root/dashboard"
keycloak_image="quay.io/keycloak/keycloak:26.8.2@sha256:c68e286cacd87b942ac1939a0a1481461109d6acd3a57dc496969a319a76bbf0"
keycloak_container="pagerless-smoke-keycloak"
keycloak_port=18080
dashboard_port=18082

for command in docker go node npm python3 curl; do
	command -v "$command" >/dev/null || {
		echo "$command is required." >&2
		exit 1
	}
done
[ -x "$dashboard_dir/node_modules/.bin/playwright" ] || {
	echo "Install the dashboard dependencies first: (cd dashboard && npm ci)" >&2
	exit 1
}
for port in "$keycloak_port" "$dashboard_port"; do
	if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
		echo "Port $port is already in use." >&2
		exit 1
	fi
done

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-smoke.XXXXXXXX")"
chmod 700 "$state"
server_log="$state/server.log"
server_pid=""
cleanup() {
	local result=$?
	if [ -n "$server_pid" ]; then
		kill "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	docker rm --force "$keycloak_container" >/dev/null 2>&1 || true
	if [ "$result" -ne 0 ]; then
		echo "Smoke test failed; the server log follows." >&2
		sed -n '1,200p' "$server_log" >&2 || true
	fi
	rm -rf "$state"
}
trap cleanup EXIT INT TERM

admin_password="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
docker run --detach --rm --name "$keycloak_container" \
	--publish "127.0.0.1:${keycloak_port}:8080" \
	--env KC_BOOTSTRAP_ADMIN_USERNAME=admin \
	--env "KC_BOOTSTRAP_ADMIN_PASSWORD=$admin_password" \
	"$keycloak_image" start-dev --http-port=8080 --hostname="http://127.0.0.1:${keycloak_port}" >/dev/null

python3 "$script_dir/keycloak.py" \
	--base-url "http://127.0.0.1:${keycloak_port}" \
	--admin-password "$admin_password" \
	--redirect "http://127.0.0.1:${dashboard_port}" \
	--state-file "$state/session.json"
unset admin_password

python3 - "$state/session.json" "$state/client-secret" <<'PY'
import json, sys
with open(sys.argv[1]) as handle:
    state = json.load(handle)
with open(sys.argv[2], "w") as handle:
    handle.write(state["clientSecret"])
PY
chmod 600 "$state/client-secret"

export KUBEBUILDER_ASSETS="$(cd "$repo_root" && go tool setup-envtest use -p path 1.37.0)"
PAGERLESS_SMOKE_BASE_URL="http://127.0.0.1:${dashboard_port}" \
PAGERLESS_SMOKE_ISSUER="http://127.0.0.1:${keycloak_port}/realms/pagerless" \
PAGERLESS_SMOKE_APPROVER="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["username"])' "$state/session.json")" \
PAGERLESS_SMOKE_CLIENT_SECRET_FILE="$state/client-secret" \
	go test -tags smoke -run TestDashboardSmoke -count=1 -timeout 20m ./internal/server \
	>"$server_log" 2>&1 &
server_pid=$!

for _ in $(seq 1 180); do
	status="$(curl --silent --output /dev/null --write-out '%{http_code}' \
		"http://127.0.0.1:${dashboard_port}/api/session" || true)"
	if [ "$status" = "401" ]; then
		break
	fi
	if ! kill -0 "$server_pid" 2>/dev/null; then
		echo "The smoke server stopped before the dashboard answered." >&2
		exit 1
	fi
	sleep 1
done
[ "$status" = "401" ] || {
	echo "The dashboard did not start on port $dashboard_port." >&2
	exit 1
}

cd "$dashboard_dir"
PAGERLESS_SMOKE_BASE_URL="http://127.0.0.1:${dashboard_port}" \
PAGERLESS_SMOKE_STATE="$state/session.json" \
	npx playwright test

wait "$server_pid"
server_pid=""
echo "Dashboard smoke test passed."

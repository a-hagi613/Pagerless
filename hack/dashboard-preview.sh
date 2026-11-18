#!/usr/bin/env bash
# Manages a live single-port Pagerless dashboard preview on the devbox:
# starts a local Keycloak container on 127.0.0.1:18180, provisions the
# pagerless realm and approver user, and runs pagerless-server on 0.0.0.0:8080
# with its single-port Keycloak realm proxy and seeded sample Triages.
# All runtime state and credentials live under /tmp/pagerless-dashboard-preview
# with mode 0700/0600; no secret is ever printed or written to the repository.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
dashboard_dir="$repo_root/dashboard"
state_dir="/tmp/pagerless-dashboard-preview"
keycloak_image="quay.io/keycloak/keycloak:26.8.2@sha256:c68e286cacd87b942ac1939a0a1481461109d6acd3a57dc496969a319a76bbf0"
keycloak_container="pagerless-preview-keycloak"
keycloak_port="${PAGERLESS_PREVIEW_KEYCLOAK_PORT:-18180}"
preview_bind="${PAGERLESS_PREVIEW_BIND:-0.0.0.0:8080}"
preview_url="${PAGERLESS_PREVIEW_URL:-http://devbox:8080}"
preview_port="${preview_bind##*:}"

pid_file="$state_dir/server.pid"
ready_file="$state_dir/ready"
session_file="$state_dir/session.json"
secret_file="$state_dir/client-secret"
credentials_file="$state_dir/credentials.env"
server_log="$state_dir/server.log"
server_bin="$state_dir/preview-server.test"

is_running() {
	[ -f "$pid_file" ] || return 1
	local pid
	pid="$(cat "$pid_file" 2>/dev/null || true)"
	[ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

stop_preview() {
	if [ -f "$pid_file" ]; then
		local pid
		pid="$(cat "$pid_file" 2>/dev/null || true)"
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill -TERM "$pid" 2>/dev/null || true
			for _ in $(seq 1 30); do
				kill -0 "$pid" 2>/dev/null || break
				sleep 0.2
			done
			kill -KILL "$pid" 2>/dev/null || true
		fi
		rm -f "$pid_file"
	fi
	docker rm --force "$keycloak_container" >/dev/null 2>&1 || true
	rm -f "$ready_file" "$server_bin"
}

status_preview() {
	local kc_running="no"
	if docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$keycloak_container"; then
		kc_running="yes"
	fi
	if is_running; then
		echo "Dashboard preview: running (PID $(cat "$pid_file"))"
		echo "Keycloak container ($keycloak_container): $kc_running"
		echo "Preview URL: $preview_url"
		echo "Credentials file (mode 0600): $credentials_file"
		return 0
	fi
	echo "Dashboard preview: stopped"
	echo "Keycloak container ($keycloak_container): $kc_running"
	return 1
}

start_preview() {
	for command in docker go node npm python3 curl; do
		command -v "$command" >/dev/null || {
			echo "$command is required." >&2
			exit 1
		}
	done

	stop_preview

	for port in "$keycloak_port" "$preview_port"; do
		if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
			echo "Port $port is already in use." >&2
			exit 1
		fi
	done

	mkdir -p "$state_dir"
	chmod 700 "$state_dir"
	rm -f "$ready_file" "$session_file" "$secret_file" "$credentials_file" "$server_log"

	if [ ! -f "$dashboard_dir/dist/index.html" ]; then
		make -C "$repo_root" dashboard-build
	fi

	local admin_password
	admin_password="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
	docker run --detach --rm --name "$keycloak_container" \
		--publish "127.0.0.1:${keycloak_port}:8080" \
		--env KC_BOOTSTRAP_ADMIN_USERNAME=admin \
		--env "KC_BOOTSTRAP_ADMIN_PASSWORD=$admin_password" \
		"$keycloak_image" start-dev --http-port=8080 --hostname="http://127.0.0.1:${keycloak_port}" >/dev/null

	python3 "$dashboard_dir/e2e/keycloak.py" \
		--base-url "http://127.0.0.1:${keycloak_port}" \
		--admin-password "$admin_password" \
		--redirect "$preview_url" \
		--state-file "$session_file"
	unset admin_password
	chmod 600 "$session_file"

	python3 - "$session_file" "$secret_file" "$credentials_file" <<'PY'
import json
import os
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    state = json.load(handle)

secret_fd = os.open(sys.argv[2], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(secret_fd, "w", encoding="utf-8") as handle:
    handle.write(state["clientSecret"])

creds_fd = os.open(sys.argv[3], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(creds_fd, "w", encoding="utf-8") as handle:
    handle.write(f"PAGERLESS_PREVIEW_USERNAME={state['username']}\n")
    handle.write(f"PAGERLESS_PREVIEW_PASSWORD={state['password']}\n")
PY

	local kubebuilder_assets
	kubebuilder_assets="$(cd "$repo_root" && go tool setup-envtest use -p path 1.37.0)"
	local approver
	approver="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["username"])' "$session_file")"

	(cd "$repo_root" && go test -tags preview -c ./internal/server -o "$server_bin")

	KUBEBUILDER_ASSETS="$kubebuilder_assets" \
	PAGERLESS_PREVIEW_BIND="$preview_bind" \
	PAGERLESS_PREVIEW_BASE_URL="$preview_url" \
	PAGERLESS_PREVIEW_ISSUER="http://127.0.0.1:${keycloak_port}/realms/pagerless" \
	PAGERLESS_PREVIEW_APPROVER="$approver" \
	PAGERLESS_PREVIEW_CLIENT_SECRET_FILE="$secret_file" \
	PAGERLESS_PREVIEW_READY_FILE="$ready_file" \
		"$server_bin" -test.run '^TestDashboardPreview$' -test.timeout 0 \
		>"$server_log" 2>&1 &
	local server_pid=$!
	echo "$server_pid" >"$pid_file"
	chmod 600 "$pid_file" "$server_log"

	for _ in $(seq 1 180); do
		if [ -f "$ready_file" ]; then
			break
		fi
		if ! kill -0 "$server_pid" 2>/dev/null; then
			echo "The preview server exited before becoming ready." >&2
			sed -n '1,120p' "$server_log" >&2 || true
			stop_preview
			exit 1
		fi
		sleep 1
	done

	if [ ! -f "$ready_file" ]; then
		echo "Timed out waiting for the dashboard preview to seed sample Triages." >&2
		sed -n '1,120p' "$server_log" >&2 || true
		stop_preview
		exit 1
	fi

	echo "Dashboard preview is ready."
	echo "Preview URL: $preview_url"
	echo "Bind address: $preview_bind"
	echo "Approver username: $approver"
	echo "Credentials file (mode 0600): $credentials_file"
}

action="${1:-status}"
case "$action" in
	start)
		start_preview
		;;
	stop)
		stop_preview
		echo "Dashboard preview stopped."
		;;
	status)
		status_preview
		;;
	*)
		echo "Usage: $0 {start|stop|status}" >&2
		exit 2
		;;
esac

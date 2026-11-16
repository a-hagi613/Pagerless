#!/usr/bin/env bash
# Prompts for the local configuration that make up needs and writes it to the
# git-ignored .env file (ADR-0015). Values already present in .env are kept,
# so the prompts run only for values that are missing.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

load_env

changed=0

ask_sandbox_url() {
	local value
	while :; do
		read -r -p "Sandbox fork URL (https://github.com/<you>/pagerless-sandbox): " value
		if [[ "$value" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/?$ ]]; then
			SANDBOX_URL="${value%/}"
			return
		fi
		echo "Enter the HTTPS URL of your Sandbox fork, for example https://github.com/octocat/pagerless-sandbox." >&2
	done
}

ask_token() {
	local label="$1" value
	while :; do
		read -r -s -p "$label: " value
		echo
		if [ -n "$value" ] && [[ ! "$value" =~ [[:space:]] ]]; then
			REPLY_TOKEN="$value"
			return
		fi
		echo "The token must not be empty and must not contain whitespace." >&2
	done
}

ask_model_provider() {
	local value
	while :; do
		read -r -p "Model provider (replay, bedrock, anthropic) [replay]: " value
		value="${value:-replay}"
		case "$value" in
		replay | bedrock | anthropic)
			MODEL_PROVIDER="$value"
			return
			;;
		esac
		echo "Choose replay, bedrock, or anthropic." >&2
	done
}

if [ -z "${SANDBOX_URL:-}" ]; then
	ask_sandbox_url
	changed=1
fi

if [ -z "${GITHUB_READ_TOKEN:-}" ]; then
	echo "Read-only fine-grained token for the Sandbox (contents: read). Input is hidden."
	ask_token "GitHub read-only token"
	GITHUB_READ_TOKEN="$REPLY_TOKEN"
	changed=1
fi

if [ -z "${GITHUB_WRITE_TOKEN:-}" ]; then
	echo "Write fine-grained token for the Sandbox (contents: write, pull requests: write). Input is hidden."
	ask_token "GitHub write token"
	GITHUB_WRITE_TOKEN="$REPLY_TOKEN"
	changed=1
fi

if [ -z "${MODEL_PROVIDER:-}" ]; then
	ask_model_provider
	changed=1
fi

if [ "$changed" -eq 0 ]; then
	echo "Using the existing .env. Delete it to be prompted again."
	exit 0
fi

umask 077
tmp="$(mktemp "$REPO_ROOT/.env.XXXXXX")"
cat >"$tmp" <<EOF
# Local configuration written by hack/configure.sh. Git-ignored; do not commit.
SANDBOX_URL='$SANDBOX_URL'
GITHUB_READ_TOKEN='$GITHUB_READ_TOKEN'
GITHUB_WRITE_TOKEN='$GITHUB_WRITE_TOKEN'
MODEL_PROVIDER='$MODEL_PROVIDER'
EOF
mv "$tmp" "$ENV_FILE"
echo "Wrote $ENV_FILE"

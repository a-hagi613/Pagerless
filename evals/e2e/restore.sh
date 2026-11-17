#!/usr/bin/env bash
# Restore Argo CD automation after a retained E2E fixture has been used.
set -euo pipefail
state="${1:-}"
if [ -z "$state" ] || [ ! -d "$state" ]; then
	echo "usage: restore.sh <retained E2E state directory>" >&2
	exit 2
fi
context="${KUBECTL_CONTEXT:-kind-pagerless}"
for app in pagerless-server pagerless-executor mcp root; do
	policy="$state/$app-sync-policy.json"
	[ -r "$policy" ] || { echo "Missing saved sync policy for $app." >&2; exit 1; }
	value="$(cat "$policy")"
	kubectl --context "$context" -n argocd patch application "$app" \
		--type=merge --patch "{\"spec\":{\"syncPolicy\":$value}}"
done

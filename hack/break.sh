#!/usr/bin/env bash
# Commits a Scenario's injection to the Sandbox. The change goes through a pull
# request with auto-merge (ADR-0011), so it merges only after the kubeconform
# check passes. The script then waits for Argo CD to sync the merge commit and
# for the Workload to report OOMKilled.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions
load_env

scenario="${1:-}"
[ -n "$scenario" ] || die "usage: make break SCENARIO=<name>, for example make break SCENARIO=oom"
patch_file="$REPO_ROOT/scenarios/$scenario/inject.patch"
[ -f "$patch_file" ] || die "unknown Scenario '$scenario': $patch_file does not exist."

require_env SANDBOX_URL GITHUB_WRITE_TOKEN
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	require_env PAGERLESS_E2E_API_URL PAGERLESS_E2E_CONTROL_URL PAGERLESS_E2E_GIT_URL PAGERLESS_E2E_TOKEN \
		PAGERLESS_E2E_REPOSITORY PAGERLESS_E2E_CA_FILE PAGERLESS_E2E_KUBECONFORM_BIN \
		GITHUB_READ_TOKEN GITHUB_WRITE_TOKEN
	require_commands git uv python3
	[ "$(uv --version | cut -d ' ' -f 2)" = "$UV_VERSION" ] || die "uv $UV_VERSION is required for the adapter client."
	[[ "$PAGERLESS_E2E_API_URL" =~ ^https://(localhost|127\.0\.0\.1):[0-9]+$ ]] ||
		die "the adapter API must use loopback HTTPS."
	[[ "$PAGERLESS_E2E_CONTROL_URL" =~ ^http://(127\.0\.0\.1|localhost):[0-9]+$ ]] ||
		die "the adapter control API must use loopback HTTP."
	[[ "$PAGERLESS_E2E_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
		die "the adapter repository must be an owner/name pair."
	[[ "$PAGERLESS_E2E_GIT_URL" =~ ^https://(localhost|127\.0\.0\.1):[0-9]+/git/$PAGERLESS_E2E_REPOSITORY\.git$ ]] ||
		die "the adapter Git URL must be a loopback HTTPS URL for the configured repository."
	[[ "$PAGERLESS_E2E_API_URL" == "${PAGERLESS_E2E_GIT_URL%%/git/*}" ]] ||
		die "the adapter Git and API URLs must use the same loopback host and port."
	[[ "$SANDBOX_URL" == "https://sandbox-adapter.pagerless-e2e.svc:8443/git/$PAGERLESS_E2E_REPOSITORY.git" ]] ||
		die "the configured Sandbox must be the isolated pagerless-e2e adapter Service."
	[[ "$PAGERLESS_E2E_TOKEN" == e2e-write-* && "$GITHUB_WRITE_TOKEN" == "$PAGERLESS_E2E_TOKEN" && "$GITHUB_READ_TOKEN" == e2e-read-* && "$GITHUB_READ_TOKEN" != "$GITHUB_WRITE_TOKEN" ]] ||
		die "the adapter requires distinct disposable e2e-read-* and e2e-write-* tokens in the isolated configuration."
	[ -r "$PAGERLESS_E2E_CA_FILE" ] || die "the adapter CA certificate is not readable."
	[ -x "$PAGERLESS_E2E_KUBECONFORM_BIN" ] || die "the pinned kubeconform binary is not executable."
	"$PAGERLESS_E2E_KUBECONFORM_BIN" -v 2>&1 | grep -q 'v0.8.0' ||
		die "kubeconform v0.8.0 is required for the Sandbox manifest check."
	adapter_host="${PAGERLESS_E2E_GIT_URL%%/git/*}"
	git_e2e() {
		local authorization
		authorization="$(PAGERLESS_E2E_TOKEN="$PAGERLESS_E2E_TOKEN" python3 -c 'import base64, os; print(base64.b64encode(("pagerless:" + os.environ["PAGERLESS_E2E_TOKEN"]).encode()).decode())')"
		GIT_CONFIG_COUNT=1 \
			GIT_CONFIG_KEY_0="http.${adapter_host}/.extraHeader" \
			GIT_CONFIG_VALUE_0="Authorization: Basic $authorization" \
			GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
			GIT_SSL_CAINFO="$PAGERLESS_E2E_CA_FILE" git -c credential.helper= "$@"
	}
else
	require_commands git gh
	gh --version | head -n 1 | grep -q "^gh version ${GH_VERSION#v} " ||
		die "gh ${GH_VERSION} is required; found: $(gh --version | head -n 1)."
fi
(cd / && git var GIT_COMMITTER_IDENT >/dev/null 2>&1) || die "git has no commit identity. Set user.name and user.email."
branch="scenario/$scenario-$(date -u +%Y%m%d%H%M%S)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	git_e2e clone --quiet "$PAGERLESS_E2E_GIT_URL" "$work/sandbox"
else
	export GH_TOKEN="$GITHUB_WRITE_TOKEN"
	slug="$(sandbox_slug)"
	git_authenticated clone --quiet "$SANDBOX_URL" "$work/sandbox"
fi
cd "$work/sandbox"
git checkout --quiet -b "$branch"
git apply "$patch_file"
git commit --quiet -am "Inject the $scenario Scenario into checkout

The Scenario patch lowers the checkout memory request and limit below the
memory the container allocates, so the kernel terminates it with OOMKilled.
Pagerless remediates the change through a pull request."
if [ "${PAGERLESS_E2E:-0}" = "1" ]; then
	git_e2e push --quiet origin "$branch"
	e2e_client=(uv run --no-project --python "$PYTHON_VERSION" "$REPO_ROOT/evals/e2e/client.py" --repository "$PAGERLESS_E2E_REPOSITORY")
	pr_number="$("${e2e_client[@]}" open --branch "$branch" --title "Inject the $scenario Scenario into checkout" \
		--body "Injects the $scenario Scenario through make break. The Sandbox manifest check runs before the adapter records success.")"
	"${e2e_client[@]}" enable --number "$pr_number"
	"$PAGERLESS_E2E_KUBECONFORM_BIN" -strict -summary -kubernetes-version 1.37.0 workloads
	"${e2e_client[@]}" pass-check --number "$pr_number" --sha "$(git rev-parse HEAD)"
	merge_sha="$("${e2e_client[@]}" wait --number "$pr_number" --timeout 180)"
	echo "The pinned kubeconform check passed and the adapter merged pull request $pr_number as $merge_sha."
else
	git_authenticated push --quiet origin "$branch"
	pr_url="$(gh pr create --repo "$slug" --base main --head "$branch" \
		--title "Inject the $scenario Scenario into checkout" \
		--body "Injects the $scenario Scenario through hack/break.sh. The pull request merges automatically once the kubeconform check passes.")"
	gh pr merge "$pr_url" --auto --squash --delete-branch >/dev/null

	echo "Opened $pr_url with auto-merge. Waiting for the merge."
	deadline=$((SECONDS + 600))
	while :; do
		state="$(gh pr view "$pr_url" --json state --jq .state)"
		if [ "$state" = MERGED ]; then
			break
		fi
		[ "$SECONDS" -lt "$deadline" ] || die "$pr_url did not merge within 10 minutes (state $state)."
		sleep 5
	done
	merge_sha="$(gh pr view "$pr_url" --json mergeCommit --jq .mergeCommit.oid)"
fi

cd "$REPO_ROOT"
echo "Merged as $merge_sha. Waiting for Argo CD to sync it."
# The Workload is expected to be unhealthy after this sync, so only the sync is awaited.
wait_for_application sandbox "$merge_sha" 300 synced

deadline=$((SECONDS + 240))
while :; do
	reasons="$(kc -n sandbox get pods -l app.kubernetes.io/name=checkout \
		-o jsonpath='{range .items[*]}{.status.containerStatuses[*].lastState.terminated.reason}{"\n"}{end}' 2>/dev/null || true)"
	if grep -q OOMKilled <<<"$reasons"; then
		break
	fi
	[ "$SECONDS" -lt "$deadline" ] || die "the checkout Workload did not report OOMKilled within 4 minutes."
	sleep 5
done

echo "The $scenario Scenario is injected. The checkout Workload reports OOMKilled."

# The OOMKilled alert reaches Alertmanager after kube-state-metrics reports the
# termination and Prometheus evaluates the rule. The Alertmanager API is read
# through the API server, so no port-forward is needed.
alerts_path="/api/v1/namespaces/monitoring/services/kube-prometheus-stack-alertmanager:http-web/proxy/api/v2/alerts?active=true"
deadline=$((SECONDS + 300))
while :; do
	alerts="$(kc get --raw "$alerts_path" 2>/dev/null || true)"
	if grep -q '"alertname":"ContainerOOMKilled"' <<<"$alerts" &&
		grep -q '"pagerless_triage":"true"' <<<"$alerts" &&
		grep -q '"deployment":"checkout"' <<<"$alerts"; then
		break
	fi
	[ "$SECONDS" -lt "$deadline" ] || die "the ContainerOOMKilled alert for checkout did not fire in Alertmanager within 5 minutes."
	sleep 5
done

echo "The ContainerOOMKilled alert is firing in Alertmanager for the checkout Workload."
echo "Restore the Sandbox with: make reset"

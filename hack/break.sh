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
require_commands git gh
gh --version | head -n 1 | grep -q "^gh version ${GH_VERSION#v} " ||
	die "gh ${GH_VERSION} is required; found: $(gh --version | head -n 1)."
(cd / && git var GIT_COMMITTER_IDENT >/dev/null 2>&1) || die "git has no commit identity. Set user.name and user.email."
export GH_TOKEN="$GITHUB_WRITE_TOKEN"

slug="$(sandbox_slug)"
branch="scenario/$scenario-$(date -u +%Y%m%d%H%M%S)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git_authenticated clone --quiet "$SANDBOX_URL" "$work/sandbox"
cd "$work/sandbox"
git checkout --quiet -b "$branch"
git apply "$patch_file"
git commit --quiet -am "Inject the $scenario Scenario into checkout

The Scenario patch lowers the checkout memory request and limit below the
memory the container allocates, so the kernel terminates it with OOMKilled.
Pagerless remediates the change through a pull request."
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

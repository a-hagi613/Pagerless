#!/usr/bin/env bash
# Installs the packaged chart on a fresh kind cluster and exercises the product
# path end to end: the real read-only Kubernetes MCP server reads a broken
# workload, the fake OpenAI-compatible model drives the agent, the service
# records the ordered transcript, redacts the workload's credential, and the
# isolated publisher opens exactly one draft pull request against the fake
# GitHub repository. The released CLI and the browser API are both exercised.
#
# Requirements: docker, kind, kubectl, helm, go, python3, curl.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands docker kind kubectl helm go python3 curl openssl

CLUSTER="${PAGERLESS_VERIFY_CLUSTER:-pagerless-verify}"
RELEASE="pagerless"
NAMESPACE="pagerless"
WORKLOAD_NAMESPACE="shop"
E2E_IMAGE="pagerless-e2e:e2e"
LOCAL_PORT="${PAGERLESS_VERIFY_PORT:-18080}"
TIMEOUT="${PAGERLESS_VERIFY_TIMEOUT:-420}"
STATE="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-e2e.XXXXXXXX")"
chmod 700 "$STATE"

cleanup() {
	if [ -n "${PORT_FORWARD_PID:-}" ]; then
		kill "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
	fi
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	rm -rf "$STATE"
}
trap cleanup EXIT

kc() { kubectl --context "kind-${CLUSTER}" "$@"; }

echo "==> creating kind cluster ${CLUSTER}"
kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --wait 180s

echo "==> building the service, agent, and integration images"
docker build --file Dockerfile.pagerless-server --build-arg "VERSION=1.0.0" --tag "$PAGERLESS_SERVER_IMAGE" .
docker build --file Dockerfile.pagerless-agent --build-arg "VERSION=1.0.0" --tag "$PAGERLESS_AGENT_IMAGE" .
docker build --file test/e2e/investigation/Dockerfile --tag "$E2E_IMAGE" .
kind load docker-image --name "$CLUSTER" "$PAGERLESS_SERVER_IMAGE" "$PAGERLESS_AGENT_IMAGE" "$E2E_IMAGE"

echo "==> creating namespaces, the broken workload, and the externally provisioned Secret"
kc create namespace "$NAMESPACE" --dry-run=client -o yaml | kc apply -f -
kc create namespace "$WORKLOAD_NAMESPACE" --dry-run=client -o yaml | kc apply -f -
kc apply -f "$REPO_ROOT/test/e2e/investigation/broken-workload.yaml"
kc apply -f "$REPO_ROOT/test/e2e/investigation/fake.yaml"
kc -n "$NAMESPACE" rollout status deployment/pagerless-fake --timeout=120s

operator_token="$(openssl rand -hex 32)"
internal_token="$(openssl rand -hex 32)"
mcp_token="$(openssl rand -hex 32)"
kc -n "$NAMESPACE" create secret generic pagerless-credentials \
	--from-literal=operator-token="$operator_token" \
	--from-literal=internal-token="$internal_token" \
	--from-literal=mcp-token="$mcp_token" \
	--from-literal=github-read-token=e2e-read-token \
	--from-literal=github-write-token=e2e-write-token \
	--from-literal=model-api-key=e2e-model-key \
	--dry-run=client -o yaml | kc apply -f -

before_workload="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o json | python3 -c 'import json,sys,hashlib; print(hashlib.sha256(json.dumps(json.load(sys.stdin),sort_keys=True).encode()).hexdigest())')"

echo "==> installing the packaged chart"
helm upgrade --install "$RELEASE" "$REPO_ROOT/charts/pagerless" \
	--kube-context "kind-${CLUSTER}" --namespace "$NAMESPACE" \
	--set existingSecret=pagerless-credentials \
	--set model.baseUrl=http://pagerless-fake:9090/v1 \
	--set model.name=fake-model \
	--set github.repository=acme/cluster \
	--set github.baseBranch=main \
	--set github.pathPrefix=apps/checkout \
	--set image.tag=1.0.0 \
	--set agentImage.tag=1.0.0 \
	--set agent.budget.seconds=180 \
	--wait --timeout "${TIMEOUT}s" >"$STATE/install.txt" 2>&1 ||
	{ kc -n "$NAMESPACE" get pods; kc -n "$NAMESPACE" describe pods; cat "$STATE/install.txt"; exit 1; }

echo "==> port-forwarding the private Service"
kc -n "$NAMESPACE" port-forward service/pagerless-server "${LOCAL_PORT}:8080" >"$STATE/port-forward.log" 2>&1 &
PORT_FORWARD_PID=$!
ready=0
for _ in $(seq 1 60); do
	if curl --silent --fail "http://127.0.0.1:${LOCAL_PORT}/healthz" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 2
done
[ "$ready" -eq 1 ] || { echo "the server did not answer through the port-forward"; cat "$STATE/port-forward.log"; exit 1; }

status=$(curl --silent -o "$STATE/ready.json" -w '%{http_code}' "http://127.0.0.1:${LOCAL_PORT}/readyz")
[ "$status" = "200" ] || {
	echo "the installation is not ready (HTTP $status):"
	cat "$STATE/ready.json"
	kc -n "$NAMESPACE" logs deployment/pagerless-server --tail=100 || true
	kc -n "$NAMESPACE" logs deployment/pagerless-agent --tail=100 || true
	kc -n "$NAMESPACE" logs deployment/pagerless-kubernetes-mcp --tail=100 || true
	kc -n "$NAMESPACE" logs deployment/pagerless-github-mcp --tail=100 || true
	exit 1
}
python3 - "$STATE/ready.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body["status"] == "ready", body
for check in body["checks"]:
    assert check["ok"], check
print("readiness:", ", ".join(c["name"] for c in body["checks"]))
PY

echo "==> proving the read-only authorization matrix"
bash "$REPO_ROOT/hack/verify-read-only-grants.sh" \
	--context "kind-${CLUSTER}" --namespace "$NAMESPACE" \
	--release "$RELEASE" --workload-namespace "$WORKLOAD_NAMESPACE"

fake_pod="$(kc -n "$NAMESPACE" get pod -l app.kubernetes.io/name=pagerless-fake -o jsonpath='{.items[0].metadata.name}')"

echo "==> asking through the released CLI"
printf '%s\n' "$operator_token" | kc -n "$NAMESPACE" exec -i "$fake_pod" -- \
	/usr/local/bin/kubectl-page "Why is checkout failing?" -n "$WORKLOAD_NAMESPACE" \
	--address "http://pagerless-server:8080" --token-file - --json \
	--follow-timeout 300s >"$STATE/cli.json"
E2E_PAGE_FILE="$STATE/page-id" python3 - "$STATE/cli.json" <<'PY'
import json, os, sys
body = json.load(open(sys.argv[1]))
assert body["status"] == "completed", body
assert body["messages"], body
assert any(m["role"] == "assistant" and m["citations"] for m in body["messages"]), body
print("cli:", body["id"], body["status"], len(body["messages"]), "messages")
open(os.environ["E2E_PAGE_FILE"], "w").write(body["id"])
PY
page_id="$(cat "$STATE/page-id")"

echo "==> opening a second investigation through the browser API"
kc -n "$NAMESPACE" exec "$fake_pod" -- env \
	E2E_BASE_URL=http://pagerless-server:8080 \
	E2E_TOKEN="$operator_token" \
	E2E_QUESTION="Why is checkout failing?" \
	E2E_NAMESPACE="$WORKLOAD_NAMESPACE" \
	/usr/local/bin/fake --assert >"$STATE/assert.json"
cat "$STATE/assert.json"

printf '%s\n' "$operator_token" | kc -n "$NAMESPACE" exec -i "$fake_pod" -- \
	/usr/local/bin/kubectl-page diff "$page_id" \
	--address "http://pagerless-server:8080" --token-file - --json >"$STATE/diff.json"
python3 - "$STATE/diff.json" <<'PY'
import json, sys
body = json.load(open(sys.argv[1]))
assert body.get("prUrl"), body
assert body["files"] and body["files"][0]["path"] == "apps/checkout/deployment.yaml", body
print("diff:", body["prUrl"], len(body["files"]), "file(s)")
PY

echo "==> asserting one draft pull request and zero merges"
kc -n "$NAMESPACE" exec "$fake_pod" -- wget -qO- http://pagerless-fake:9091/_test/pulls >"$STATE/pulls.json"
kc -n "$NAMESPACE" exec "$fake_pod" -- wget -qO- http://pagerless-fake:9091/_test/merges >"$STATE/merges.json"
python3 - "$STATE/pulls.json" "$STATE/merges.json" <<'PY'
import json, sys
pulls = json.load(open(sys.argv[1]))
merges = json.load(open(sys.argv[2]))
assert len(pulls) == 1, pulls
pull = pulls[0]
assert pull["draft"] is True, pull
assert pull["base"] == "main" and pull["head"] != "main", pull
assert "checkout" in pull["title"].lower(), pull
assert merges["merges"] == 0, merges
print("pull request:", pull["head"], "->", pull["base"], "draft", pull["draft"])
PY

echo "==> asserting the workload was not changed"
after_workload="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o json | python3 -c 'import json,sys,hashlib; print(hashlib.sha256(json.dumps(json.load(sys.stdin),sort_keys=True).encode()).hexdigest())')"
[ "$before_workload" = "$after_workload" ] || die "the cluster workload changed; Pagerless must not mutate a cluster."

echo "==> proving readiness reports a broken model and recovers"
helm upgrade "$RELEASE" "$REPO_ROOT/charts/pagerless" \
	--kube-context "kind-${CLUSTER}" --namespace "$NAMESPACE" --reuse-values \
	--set model.baseUrl=http://pagerless-fake:9090/v1-broken --wait --timeout "${TIMEOUT}s" >/dev/null
degraded=0
for _ in $(seq 1 60); do
	status=$(curl --silent -o "$STATE/degraded.json" -w '%{http_code}' "http://127.0.0.1:${LOCAL_PORT}/readyz" || true)
	if [ "$status" = "503" ] && grep -q '"name":"model"' "$STATE/degraded.json"; then
		degraded=1
		break
	fi
	sleep 3
done
[ "$degraded" -eq 1 ] || { echo "readiness did not report the broken model:"; cat "$STATE/degraded.json"; exit 1; }
helm upgrade "$RELEASE" "$REPO_ROOT/charts/pagerless" \
	--kube-context "kind-${CLUSTER}" --namespace "$NAMESPACE" --reuse-values \
	--set model.baseUrl=http://pagerless-fake:9090/v1 --wait --timeout "${TIMEOUT}s" >/dev/null
for _ in $(seq 1 60); do
	status=$(curl --silent -o "$STATE/ready.json" -w '%{http_code}' "http://127.0.0.1:${LOCAL_PORT}/readyz" || true)
	[ "$status" = "200" ] && break
	sleep 3
done
[ "$status" = "200" ] || { echo "the installation did not recover:"; cat "$STATE/ready.json"; exit 1; }

if [ "${PAGERLESS_E2E_BROWSER:-0}" = "1" ]; then
	echo "==> running the dashboard browser suite against the installed backend"
	( cd "$REPO_ROOT/dashboard" && npm ci && npx playwright install --with-deps chromium )
	PAGERLESS_PORTAL_URL="http://127.0.0.1:${LOCAL_PORT}" \
		PAGERLESS_E2E_TOKEN="$operator_token" \
		make -C "$REPO_ROOT" dashboard-e2e
fi

echo
echo "kind integration: CLI, browser API, Kubernetes MCP, redaction, readiness recovery, RBAC, and draft PR verified."

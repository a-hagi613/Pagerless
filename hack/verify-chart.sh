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
	if [ "${PAGERLESS_KEEP_CLUSTER:-0}" = "1" ]; then
		echo "keeping kind cluster ${CLUSTER} for inspection"
		return
	fi
	rm -f "$REPO_ROOT/dashboard/e2e/real-backend.generated.spec.ts"
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

before_uid="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o jsonpath='{.metadata.uid}')"
before_spec="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o jsonpath='{.spec}' | sha256sum | cut -d' ' -f1)"

cat >"$STATE/values.yaml" <<'EOF'
# The fake model and GitHub endpoints run inside this cluster on test ports.
networkPolicy:
  extraEgress:
    - to:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: pagerless-fake
EOF

echo "==> installing the packaged chart"
helm upgrade --install "$RELEASE" "$REPO_ROOT/charts/pagerless" \
	--values "$STATE/values.yaml" \
	--kube-context "kind-${CLUSTER}" --namespace "$NAMESPACE" \
	--set existingSecret=pagerless-credentials \
	--set model.baseUrl=http://pagerless-fake:9090/v1 \
	--set model.name=fake-model \
	--set model.allowInsecure=true \
	--set github.repository=acme/cluster \
	--set github.baseBranch=main \
	--set github.apiUrl=http://pagerless-fake:9091 \
	--set github.pathPrefix=apps/checkout \
	--set image.tag=1.0.0 \
	--set agentImage.tag=1.0.0 \
	--set agent.budget.seconds=180 \
	--wait --timeout "${TIMEOUT}s" >"$STATE/install.txt" 2>&1 ||
	{
		kc -n "$NAMESPACE" get pods
		kc -n "$NAMESPACE" port-forward service/pagerless-server 18081:8080 >/dev/null 2>&1 &
		DIAG_PID=$!
		sleep 3
		curl --silent "http://127.0.0.1:18081/readyz" || true
		kill "$DIAG_PID" >/dev/null 2>&1 || true
		kc -n "$NAMESPACE" describe pods
		for deployment in pagerless-server pagerless-agent pagerless-kubernetes-mcp pagerless-github-mcp; do
			echo "==> logs ${deployment}"
			kc -n "$NAMESPACE" logs "deployment/${deployment}" --tail=80 || true
		done
		cat "$STATE/install.txt"
		exit 1
	}

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
	E2E_PAGE_ID="$page_id" \
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
after_uid="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o jsonpath='{.metadata.uid}')"
after_spec="$(kc -n "$WORKLOAD_NAMESPACE" get deployment checkout -o jsonpath='{.spec}' | sha256sum | cut -d' ' -f1)"
[ "$before_uid" = "$after_uid" ] || die "the workload was replaced; Pagerless must not mutate a cluster."
[ "$before_spec" = "$after_spec" ] || die "the workload spec changed; Pagerless must not mutate a cluster."

if [ "${PAGERLESS_E2E_NEGATIVE:-1}" = "1" ]; then
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

fi

if [ "${PAGERLESS_E2E_BROWSER:-0}" = "1" ]; then
	echo "==> opening the embedded dashboard against the installed backend"
	# The generated spec runs against the real service through the same client
	# code the embedded dashboard ships. It opens the CLI-created page, its
	# citation and evidence, the proposal diff, a reload (resume), and a
	# follow-up. It is generated here so the fixture suite is never pointed at a
	# real installation.
	cat >"$REPO_ROOT/dashboard/e2e/real-backend.generated.spec.ts" <<'SPEC'
import { expect, test, type Page, type TestInfo } from "@playwright/test";

const token = process.env.PAGERLESS_E2E_TOKEN ?? "";
const title = process.env.PAGERLESS_E2E_PAGE_TITLE ?? "Why is checkout failing?";

async function history(page: Page, testInfo: TestInfo) {
  if (testInfo.project.name.startsWith("mobile")) {
    await page.getByRole("button", { name: "Open investigation history" }).click();
    return page.getByRole("dialog");
  }
  return page.getByRole("region", { name: "Investigation history" }).first();
}

test("opens the CLI page, its evidence, the diff, and a follow-up", async ({ page }, testInfo) => {
  expect(token).not.toBe("");
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Pagerless" })).toBeVisible();
  await page.locator("#operator-token").fill(token);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Ask the cluster a question" })).toBeVisible();

  const scope = await history(page, testInfo);
  await scope.getByRole("link", { name: title }).first().click();

  const answers = page.getByRole("article", { name: "Pagerless answer" });
  await expect(answers.first()).toContainText("no memory limit");
  await page.getByRole("button", { name: "Open evidence for call-1" }).click();
  const evidence = page.getByRole("dialog", { name: "Tool evidence" });
  await expect(evidence).toContainText("resources_get");
  await expect(evidence).toContainText("[redacted:credential]");
  await evidence.getByRole("button", { name: "Close" }).click();

  const proposal = page.getByRole("region", { name: "Proposed change" });
  await expect(proposal).toBeVisible();
  await expect(proposal.getByRole("link", { name: "Open draft pull request" })).toHaveAttribute(
    "href",
    /pull\//,
  );

  // A reload resumes the same page from the durable cursor.
  await page.reload();
  await expect(page.getByRole("article", { name: "Pagerless answer" }).first()).toContainText(
    "no memory limit",
  );

  await page
    .getByRole("form", { name: "Investigation composer" })
    .getByRole("textbox")
    .fill("What changed?");
  await page.getByRole("button", { name: "Send follow-up" }).click();
  await expect(page.getByRole("article", { name: "Pagerless answer" })).toHaveCount(2, {
    timeout: 180_000,
  });
});
SPEC
	if [ ! -d "$REPO_ROOT/dashboard/node_modules" ]; then
		( cd "$REPO_ROOT/dashboard" && npm ci )
	fi
	(
		cd "$REPO_ROOT/dashboard"
		PAGERLESS_PORTAL_URL="http://127.0.0.1:${LOCAL_PORT}" \
			PAGERLESS_E2E_TOKEN="$operator_token" \
			PAGERLESS_E2E_PAGE_TITLE="Why is checkout failing?" \
			npx playwright test e2e/real-backend.generated.spec.ts
	)
fi

echo
if [ "${PAGERLESS_E2E_NEGATIVE:-1}" = "1" ]; then
	echo "kind integration: CLI, browser API, Kubernetes MCP, redaction, readiness recovery, RBAC, and draft PR verified."
else
	echo "kind integration: CLI, browser API, Kubernetes MCP, redaction, RBAC, and draft PR verified."
	echo "negative readiness probe: skipped by PAGERLESS_E2E_NEGATIVE=0; it is required in CI."
fi

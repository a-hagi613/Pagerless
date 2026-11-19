#!/usr/bin/env bash
# Proves the standalone chart on a real cluster.
#
# Creates a fresh kind cluster, builds pagerless-server from this checkout,
# installs charts/pagerless, waits for the pod to become Ready, and then asks
# the API server's own authorizer what the server ServiceAccount may do. Every
# check is the product's defining constraint: the cluster identity reads, and it
# cannot write to a workload, Event, or Node.
#
# The matrix itself is hack/verify-read-only-grants.sh, which the release job
# also runs against the chart published to GHCR; here it runs in the managed
# shape, with the Secret the chart creates.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands docker kind kubectl helm

CLUSTER="${PAGERLESS_VERIFY_CLUSTER:-pagerless-verify}"
IMAGE_REPOSITORY="${PAGERLESS_VERIFY_IMAGE:-pagerless-server}"
IMAGE_TAG="${PAGERLESS_VERIFY_TAG:-verify}"
TIMEOUT="${PAGERLESS_VERIFY_TIMEOUT:-180s}"

cleanup() {
	if [ -n "${PORTAL_PF_PID:-}" ]; then
		kill "$PORTAL_PF_PID" >/dev/null 2>&1 || true
	fi
	kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	die "kind cluster '$CLUSTER' already exists; delete it or set PAGERLESS_VERIFY_CLUSTER."
fi

echo "Creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --wait 120s

echo "Building $IMAGE_REPOSITORY:$IMAGE_TAG"
docker build --file "$REPO_ROOT/Dockerfile.pagerless-server" \
	--tag "$IMAGE_REPOSITORY:$IMAGE_TAG" "$REPO_ROOT"
kind load docker-image --name "$CLUSTER" "$IMAGE_REPOSITORY:$IMAGE_TAG"

echo "Installing charts/pagerless"
helm upgrade --install pagerless "$REPO_ROOT/charts/pagerless" \
	--kube-context "kind-$CLUSTER" \
	--namespace pagerless --create-namespace \
	--set "images.server.repository=$IMAGE_REPOSITORY" \
	--set "images.server.tag=$IMAGE_TAG" \
	--wait --timeout "$TIMEOUT"

kubectl --context "kind-$CLUSTER" -n pagerless rollout status deploy/pagerless-server --timeout="$TIMEOUT"
ready="$(kubectl --context "kind-$CLUSTER" -n pagerless get pods \
	--selector app.kubernetes.io/name=pagerless-server \
	--output jsonpath='{.items[0].status.containerStatuses[0].ready}')"
[ "$ready" = "true" ] || die "pagerless-server did not become ready."

echo "Checking the read-only authorization matrix for the managed credentials Secret"
bash "$REPO_ROOT/hack/verify-read-only-grants.sh" \
	--shape managed --context "kind-$CLUSTER" \
	--namespace pagerless --service-account pagerless-server

echo "Checking that a CLI decision and a Portal decision record the same Page"

# The plugin the operator installs, built from this checkout, decides one Page through the Kubernetes API.
( cd "$REPO_ROOT" && go build -trimpath -o "$REPO_ROOT/bin/kubectl-page" ./cmd/kubectl-page )
CLI="$REPO_ROOT/bin/kubectl-page"

BOOTSTRAP_TOKEN="$(kubectl --context "kind-$CLUSTER" -n pagerless get secret pagerless-admin-secret \
	--output jsonpath='{.data.token}' | base64 -d)"
PORTAL_PORT="${PAGERLESS_VERIFY_PORTAL_PORT:-18080}"
kubectl --context "kind-$CLUSTER" -n pagerless port-forward svc/pagerless-server "$PORTAL_PORT:8080" \
	>/dev/null 2>&1 &
PORTAL_PF_PID=$!
for _ in $(seq 1 60); do
	if curl --fail --silent --show-error "http://127.0.0.1:${PORTAL_PORT}/api/version" >/dev/null 2>&1; then
		break
	fi
	sleep 1
done
curl --fail --silent --show-error "http://127.0.0.1:${PORTAL_PORT}/api/version" >/dev/null ||
	die "the Portal did not answer on 127.0.0.1:${PORTAL_PORT}."

COOKIE_JAR="$(mktemp)"
PORTAL_BODY="$(mktemp)"
curl --fail --silent --show-error --cookie-jar "$COOKIE_JAR" \
	--header "Content-Type: application/json" \
	--data "{\"token\":\"${BOOTSTRAP_TOKEN}\"}" \
	"http://127.0.0.1:${PORTAL_PORT}/api/auth/login" >/dev/null
CSRF="$(curl --fail --silent --show-error --cookie "$COOKIE_JAR" \
	"http://127.0.0.1:${PORTAL_PORT}/api/session" | jq --raw-output .csrfToken)"
[ -n "$CSRF" ] && [ "$CSRF" != "null" ] || die "the Portal session has no CSRF token."

PROPOSAL_HASH="$(printf 'portal-parity-proposal' | sha256sum | cut -d' ' -f1)"
EXPIRES_AT="$(date --utc --date='+1 hour' +%Y-%m-%dT%H:%M:%SZ)"

# create_page makes one Page whose Target is an existing Deployment. The Page starts Pending; the status
# is then patched to Proposed, which is the one state a decision accepts.
create_page() {
	local name="$1" target="$2"
	# Each Page carries its own Fingerprint: two Pages that share one are
	# duplicates of the same failure, and the controller correctly supersedes
	# the second rather than letting both be decided.
	local fingerprint
	fingerprint="$(printf 'portal-parity-page-%s' "$name" | sha256sum | cut -d' ' -f1)"
	cat <<PAGE | kubectl --context "kind-$CLUSTER" apply --filename - >/dev/null
apiVersion: pagerless.dev/v1alpha1
kind: Page
metadata:
  name: ${name}
  namespace: pagerless
spec:
  target:
    apiVersion: apps/v1
    kind: Deployment
    namespace: sandbox
    name: ${target}
  fingerprint: ${fingerprint}
  trigger:
    source: CLI
PAGE
}

propose_page() {
	local name="$1"
	kubectl --context "kind-$CLUSTER" --namespace pagerless patch page "$name" \
		--subresource=status --type=merge --patch "$(cat <<STATUS
{"status":{"phase":"Proposed","proposal":{"action":"Scale",
"parameters":{"scale":{"replicas":2}},"hash":"${PROPOSAL_HASH}","attempt":1,
"expiresAt":"${EXPIRES_AT}"}}}
STATUS
	)" >/dev/null
}

# The parity Pages target a Deployment in the demo namespace the showcase uses,
# so the namespace has to exist before the Target does.
kubectl --context "kind-$CLUSTER" create namespace sandbox >/dev/null 2>&1 || true
kubectl --context "kind-$CLUSTER" --namespace sandbox create deployment checkout \
	--image=registry.invalid/checkout:1.0 >/dev/null
create_page parity-cli checkout
create_page parity-portal checkout

# Decide the same Proposal through each surface. The controller owns the status
# subresource, so a decision is only durable once it has observed the Page as
# Proposed: patch, wait for the controller to settle, then decide. Deciding while
# the status write is still in flight lets that write clobber the decision.
wait_proposed() {
	local name="$1"
	for _ in $(seq 1 30); do
		if [ "$(kubectl --context "kind-$CLUSTER" --namespace pagerless \
			get page "$name" --output jsonpath='{.status.phase}')" = Proposed ]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

cli_decided=no
portal_decided=no
for _ in $(seq 1 10); do
	if [ "$cli_decided" = no ]; then
		propose_page parity-cli
		wait_proposed parity-cli || true
		if "$CLI" --context "kind-$CLUSTER" approve parity-cli --yes \
			--reason "parity check" --by admin >/dev/null 2>&1; then
			cli_decided=yes
		fi
	fi
	if [ "$portal_decided" = no ]; then
		propose_page parity-portal
		wait_proposed parity-portal || true
		# The controller restates the Proposal hash as it reconciles, so the hash the
		# approver is shown is the one on the Page now, not the one this script sent.
		shown_hash="$(kubectl --context "kind-$CLUSTER" --namespace pagerless \
			get page parity-portal --output jsonpath='{.status.proposal.hash}')"
		if curl --fail-with-body --silent --show-error --cookie "$COOKIE_JAR" \
			--header "Content-Type: application/json" --header "X-CSRF-Token: ${CSRF}" \
			--data "{\"decision\":\"Approved\",\"proposalHash\":\"${shown_hash}\",\"reason\":\"parity check\"}" \
			--output "${PORTAL_BODY}" \
			"http://127.0.0.1:${PORTAL_PORT}/api/pages/parity-portal/decision" >/dev/null 2>&1; then
			portal_decided=yes
		else
			echo "portal decision attempt: $(cat "${PORTAL_BODY}")" >&2
		fi
	fi
	if [ "$cli_decided" = yes ] && [ "$portal_decided" = yes ]; then
		break
	fi
	sleep 1
done
[ "$cli_decided" = yes ] || die "the CLI could not approve parity-cli."
[ "$portal_decided" = yes ] || die "the Portal API could not approve parity-portal."

# Wait for the controller to reconcile both annotation sets into status.decision.
record() {
	kubectl --context "kind-$CLUSTER" --namespace pagerless get page "$1" --output json |
		jq --compact-output '.status.decision // {}'
}
for _ in $(seq 1 60); do
	cli_record="$(record parity-cli)"
	portal_record="$(record parity-portal)"
	if [ "$(jq --raw-output '.outcome // ""' <<<"$cli_record")" = Approved ] &&
		[ "$(jq --raw-output '.outcome // ""' <<<"$portal_record")" = Approved ]; then
		break
	fi
	sleep 1
done

for field in outcome proposalHash decidedBy reason; do
	cli_value="$(jq --raw-output ".${field} // \"\"" <<<"$cli_record")"
	portal_value="$(jq --raw-output ".${field} // \"\"" <<<"$portal_record")"
	[ "$cli_value" = "$portal_value" ] ||
		die "status.decision.${field} differs: CLI '${cli_value}', Portal '${portal_value}'."
done
cli_shown_hash="$(kubectl --context "kind-$CLUSTER" --namespace pagerless \
	get page parity-cli --output jsonpath='{.status.proposal.hash}')"
[ "$(jq --raw-output '.proposalHash' <<<"$cli_record")" = "$cli_shown_hash" ] ||
	die "the CLI decision is not bound to the displayed Proposal hash."

rm -f "$COOKIE_JAR" "$PORTAL_BODY"

echo "The read-only boundary holds, both decision surfaces record the same Page, and pagerless-server is Ready."

#!/usr/bin/env bash
# Proves the standalone chart on a real cluster.
#
# Creates a fresh kind cluster, builds pagerless-server from this checkout,
# installs charts/pagerless, waits for the pod to become Ready, and then asks
# the API server's own authorizer what the server ServiceAccount may do. Every
# check is the product's defining constraint: the cluster identity reads, and it
# cannot write to a workload, Event, or Node.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
load_versions

require_commands docker kind kubectl helm

CLUSTER="${PAGERLESS_VERIFY_CLUSTER:-pagerless-verify}"
IMAGE_REPOSITORY="${PAGERLESS_VERIFY_IMAGE:-pagerless-server}"
IMAGE_TAG="${PAGERLESS_VERIFY_TAG:-verify}"
SERVICE_ACCOUNT="system:serviceaccount:pagerless:pagerless-server"
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

echo "Checking the read-only authorization matrix as $SERVICE_ACCOUNT"
failures=0

# can_i asserts one authorization answer for a resource or subresource.
can_i() {
	local want="$1" verb="$2" resource="$3" namespace="${4:-pagerless}"
	local args=(auth can-i "$verb" "$resource")
	if [ -n "$namespace" ]; then
		args=(-n "$namespace" "${args[@]}")
	fi
	local got
	got="$(kubectl --context "kind-$CLUSTER" "${args[@]}" --as="$SERVICE_ACCOUNT" 2>/dev/null || true)"
	if [ "$got" != "$want" ]; then
		echo "FAIL: auth can-i $verb $resource in $namespace = '$got', want '$want'" >&2
		failures=$((failures + 1))
	fi
}

# can_i_name asserts one authorization answer for a named object, which
# `kubectl auth can-i` cannot express because a slash means a subresource.
can_i_name() {
	local want="$1" verb="$2" resource="$3" name="$4" namespace="${5:-pagerless}"
	local review response allowed
	review=$(printf '{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"%s","resourceAttributes":{"namespace":"%s","verb":"%s","resource":"%s","name":"%s"}}}' \
		"$SERVICE_ACCOUNT" "$namespace" "$verb" "$resource" "$name")
	response="$(printf '%s' "$review" |
		kubectl --context "kind-$CLUSTER" create --raw /apis/authorization.k8s.io/v1/subjectaccessreviews -f -)"
	allowed=no
	case "$response" in
	*'"allowed":true'*) allowed=yes ;;
	esac
	if [ "$allowed" != "$want" ]; then
		echo "FAIL: $verb $resource $namespace/$name = $allowed, want $want" >&2
		failures=$((failures + 1))
	fi
}

can_i yes get pods sandbox
can_i yes list deployments sandbox
can_i yes get pods/log sandbox
can_i yes list events sandbox
can_i yes get pages pagerless
can_i yes patch pages pagerless
can_i_name yes get secrets pagerless-credentials
can_i no update deployments sandbox
can_i no patch statefulsets sandbox
can_i no create daemonsets sandbox
can_i no delete pods sandbox
can_i no delete nodes ""
can_i no create events sandbox
can_i no create pods/exec sandbox
can_i no create pods/attach sandbox
can_i no create pods/portforward sandbox
can_i no create serviceaccounts/token sandbox
can_i no list secrets pagerless
can_i_name no get secrets some-other-secret
can_i no create secrets pagerless

if [ "$failures" -ne 0 ]; then
	die "$failures authorization check(s) failed."
fi

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
curl --fail --silent --show-error --cookie-jar "$COOKIE_JAR" \
	--header "Content-Type: application/json" \
	--data "{\"token\":\"${BOOTSTRAP_TOKEN}\"}" \
	"http://127.0.0.1:${PORTAL_PORT}/api/auth/login" >/dev/null
CSRF="$(curl --fail --silent --show-error --cookie "$COOKIE_JAR" \
	"http://127.0.0.1:${PORTAL_PORT}/api/session" | jq --raw-output .csrfToken)"
[ -n "$CSRF" ] && [ "$CSRF" != "null" ] || die "the Portal session has no CSRF token."

PAGE_HASH="$(printf 'portal-parity-page' | sha256sum | cut -d' ' -f1)"
PROPOSAL_HASH="$(printf 'portal-parity-proposal' | sha256sum | cut -d' ' -f1)"
EXPIRES_AT="$(date --utc --date='+1 hour' +%Y-%m-%dT%H:%M:%SZ)"

# create_page makes one Page whose Target is an existing Deployment. The Page starts Pending; the status
# is then patched to Proposed, which is the one state a decision accepts.
create_page() {
	local name="$1" target="$2"
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
  fingerprint: ${PAGE_HASH}
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

kubectl --context "kind-$CLUSTER" --namespace sandbox create deployment checkout \
	--image=registry.invalid/checkout:1.0 >/dev/null
create_page parity-cli checkout
create_page parity-portal checkout

# Decide the same Proposal through each surface. The status patch races the controller's own initial
# transition, so both attempts are retried until each surface has recorded its decision.
cli_decided=no
portal_decided=no
for _ in $(seq 1 30); do
	if [ "$cli_decided" = no ]; then
		propose_page parity-cli
		if "$CLI" --context "kind-$CLUSTER" approve parity-cli --yes \
			--reason "parity check" --by admin >/dev/null 2>&1; then
			cli_decided=yes
		fi
	fi
	if [ "$portal_decided" = no ]; then
		propose_page parity-portal
		if curl --fail --silent --show-error --cookie "$COOKIE_JAR" \
			--header "Content-Type: application/json" --header "X-CSRF-Token: ${CSRF}" \
			--data "{\"decision\":\"approve\",\"proposalHash\":\"${PROPOSAL_HASH}\",\"reason\":\"parity check\"}" \
			"http://127.0.0.1:${PORTAL_PORT}/api/pages/parity-portal/decision" >/dev/null 2>&1; then
			portal_decided=yes
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
[ "$(jq --raw-output '.proposalHash' <<<"$cli_record")" = "$PROPOSAL_HASH" ] ||
	die "the CLI decision is not bound to the displayed Proposal hash."

rm -f "$COOKIE_JAR"

echo "The read-only boundary holds, both decision surfaces record the same Page, and pagerless-server is Ready."

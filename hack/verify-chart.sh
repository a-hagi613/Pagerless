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

echo "The read-only boundary holds, and pagerless-server is Ready."

#!/usr/bin/env bash
# Proves the read-only authorization matrix of an installed Pagerless release.
#
# The chart renders its credentials-Secret rule only when
# credentials.existingSecret is empty; with it set, the server is granted no
# Secret at all and the operator mounts the Secret instead. Every other rule is
# the same for both install shapes, so one list runs against either: this script
# takes the shape as an argument and derives the named-Secret expectation from
# it. hack/verify-chart.sh runs the managed shape against a locally built chart,
# and the release workflow runs the existing shape against the chart it
# published to GHCR, so the two installs cannot drift apart.
#
# Usage: verify-read-only-grants.sh --shape managed|existing [options]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
	cat <<'USAGE'
Usage: verify-read-only-grants.sh --shape managed|existing [options]

  --shape managed|existing  managed: the chart owns the credentials Secret and
                            grants the server get, patch, and update on it.
                            existing: credentials.existingSecret names the
                            Secret, so the chart renders no Secret rule and the
                            server must be denied every verb on it.
  --context CONTEXT         kubeconfig context; defaults to the current context.
  --namespace NAMESPACE     release namespace; defaults to pagerless.
  --service-account NAME    server ServiceAccount; defaults to pagerless-server.
  --secret-name NAME        credentials Secret; defaults to pagerless-credentials.
  --target-namespace NAME   namespace the workload checks name; defaults to sandbox.

The target namespace only has to be named: the API server answers from RBAC, so
it needs no workload and does not have to exist.
USAGE
}

require_commands kubectl

shape=""
kubectl_args=()
namespace="pagerless"
service_account="pagerless-server"
secret_name="pagerless-credentials"
target_namespace="sandbox"

while [ "$#" -gt 0 ]; do
	case "$1" in
	--shape) shape="${2:-}"; shift 2 ;;
	--context)
		kubectl_args=(--context "${2:-}")
		shift 2
		;;
	--namespace) namespace="${2:-}"; shift 2 ;;
	--service-account) service_account="${2:-}"; shift 2 ;;
	--secret-name) secret_name="${2:-}"; shift 2 ;;
	--target-namespace) target_namespace="${2:-}"; shift 2 ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument $1; run with --help for the options." ;;
	esac
done

case "$shape" in
managed | existing) ;;
*) die "--shape must be managed or existing." ;;
esac

user="system:serviceaccount:${namespace}:${service_account}"
failures=0

# fail names the verb and the resource whose answer was wrong, so a widened rule
# is readable from the job log without a second query.
fail() {
	local want="$1" verb="$2" resource="$3" where="$4"
	if [ "$want" = no ]; then
		echo "FAIL: the server is allowed '$verb $resource' in $where; it must be denied" >&2
	else
		echo "FAIL: the server is denied '$verb $resource' in $where; it must be allowed" >&2
	fi
	failures=$((failures + 1))
}

# can_i asserts one authorization answer for a resource. An empty namespace asks
# about the cluster scope.
can_i() {
	local want="$1" verb="$2" resource="$3" scope="${4-$target_namespace}"
	local args=(auth can-i "$verb" "$resource")
	local where="namespace $scope"
	if [ -n "$scope" ]; then
		args=(-n "$scope" "${args[@]}")
	else
		where="the cluster scope"
	fi
	local got
	got="$(kubectl "${kubectl_args[@]}" "${args[@]}" --as="$user" 2>/dev/null || true)"
	if [ "$got" = "$want" ]; then
		echo "ok: $verb $resource ($where) = $got"
	else
		fail "$want" "$verb" "$resource" "$where"
	fi
}

# sar_answer asks the API server's authorizer directly. A SubjectAccessReview
# names the resource, the subresource, and the object exactly, which `kubectl
# auth can-i` cannot do: it reads "pods/log" as the pod named log, so a slash
# form proves nothing about the subresource.
sar_answer() {
	local verb="$1" resource="$2" subresource="$3" name="$4" scope="$5"
	local review response
	review=$(printf '{"apiVersion":"authorization.k8s.io/v1","kind":"SubjectAccessReview","spec":{"user":"%s","resourceAttributes":{"namespace":"%s","verb":"%s","resource":"%s","subresource":"%s","name":"%s"}}}' \
		"$user" "$scope" "$verb" "$resource" "$subresource" "$name")
	response="$(printf '%s' "$review" |
		kubectl "${kubectl_args[@]}" create --raw /apis/authorization.k8s.io/v1/subjectaccessreviews -f -)"
	case "$response" in
	*'"allowed":true'*) echo yes ;;
	*) echo no ;;
	esac
}

# can_i_subresource asserts one authorization answer for a subresource.
can_i_subresource() {
	local want="$1" verb="$2" resource="$3" subresource="$4" scope="${5:-$target_namespace}"
	local allowed
	allowed="$(sar_answer "$verb" "$resource" "$subresource" "" "$scope")"
	if [ "$allowed" = "$want" ]; then
		echo "ok: $verb $resource/$subresource (namespace $scope) = $allowed"
	else
		fail "$want" "$verb" "$resource/$subresource" "namespace $scope"
	fi
}

# can_i_named asserts one authorization answer for a named object, which
# `kubectl auth can-i` cannot express either.
can_i_named() {
	local want="$1" verb="$2" resource="$3" name="$4" scope="${5:-$namespace}"
	local allowed
	allowed="$(sar_answer "$verb" "$resource" "" "$name" "$scope")"
	if [ "$allowed" = "$want" ]; then
		echo "ok: $verb $resource (namespace $scope, name $name) = $allowed"
	else
		fail "$want" "$verb" "$resource" "namespace $scope, name $name"
	fi
}

# secret_expectation answers whether this shape grants the server one verb on the
# named credentials Secret. The chart's rule carries get, patch, and update with
# resourceNames; create, list, and watch are never granted, because
# resourceNames cannot constrain them.
secret_expectation() {
	local verb="$1"
	if [ "$shape" = managed ]; then
		case "$verb" in
		get | patch | update) echo yes ;;
		*) echo no ;;
		esac
		return 0
	fi
	echo no
}

echo "Checking the read-only authorization matrix for the $shape credentials-Secret shape as $user"

# The server reads the cluster. These positives prove the matrix is not
# trivially denying everything.
can_i yes get pods
can_i yes list deployments
can_i_subresource yes get pods log
can_i yes list events
can_i yes get pages "$namespace"
can_i yes patch pages "$namespace"

# No write verb on a workload, an Event, or a Node.
can_i no create deployments
can_i no update deployments
can_i no delete deployments
can_i no create statefulsets
can_i no patch statefulsets
can_i no delete statefulsets
can_i no create daemonsets
can_i no delete pods
can_i no create events
can_i no delete nodes ""

# No subresource that would reach a container, a port, or a kubelet binding.
can_i_subresource no create pods exec
can_i_subresource no create pods attach
can_i_subresource no create pods portforward
can_i_subresource no create pods binding
can_i_subresource no update pods ephemeralcontainers
can_i_subresource no create pods proxy
can_i_subresource no get pods proxy
can_i_subresource no create serviceaccounts token

# No impersonation and no privilege escalation.
can_i no impersonate serviceaccounts
can_i no escalate roles
can_i no bind roles

# The credentials Secret is the only Secret rule the chart can render, and the
# shape decides whether it renders.
can_i no create secrets "$namespace"
can_i no list secrets "$namespace"
can_i no watch secrets "$namespace"
can_i_named no get secrets some-other-secret "$namespace"
for verb in get patch update; do
	can_i_named "$(secret_expectation "$verb")" "$verb" secrets "$secret_name" "$namespace"
done

if [ "$failures" -ne 0 ]; then
	die "$failures authorization check(s) failed for the $shape credentials-Secret shape."
fi

echo "All read-only authorization checks hold for the $shape credentials-Secret shape."

#!/usr/bin/env bash
# Proves the authorization boundary of an installed release with the API
# server's own authorizer. The Kubernetes integration reads approved resources
# and nothing else; the server and agent identities hold no cluster permission
# at all.
#
# Usage: verify-read-only-grants.sh [options]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

CONTEXT="${PAGERLESS_KUBE_CONTEXT:-}"
NAMESPACE="${PAGERLESS_RELEASE_NAMESPACE:-pagerless}"
RELEASE="${PAGERLESS_RELEASE_NAME:-pagerless}"
KUBERNETES_SA="${PAGERLESS_KUBERNETES_SA:-${RELEASE}-kubernetes-mcp}"
SERVER_SA="${PAGERLESS_SERVER_SA:-${RELEASE}-server}"
AGENT_SA="${PAGERLESS_AGENT_SA:-${RELEASE}-agent}"
WORKLOAD_NAMESPACE="${PAGERLESS_WORKLOAD_NAMESPACE:-shop}"

usage() {
	cat <<'EOF'
Usage: verify-read-only-grants.sh [options]

  --context CONTEXT           kubeconfig context (defaults to the current one)
  --namespace NAMESPACE       release namespace (default pagerless)
  --release NAME              Helm release name (default pagerless)
  --workload-namespace NAME   namespace the read grant is checked against (default shop)
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--context) CONTEXT="$2"; shift 2 ;;
	--namespace) NAMESPACE="$2"; shift 2 ;;
	--release) RELEASE="$2"; shift 2 ;;
	--workload-namespace) WORKLOAD_NAMESPACE="$2"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

kube() {
	if [ -n "$CONTEXT" ]; then
		kubectl --context "$CONTEXT" "$@"
	else
		kubectl "$@"
	fi
}

failures=0
can_i() {
	local service_account="$1" verb="$2" resource="$3" namespace="${4:-}"
	local scope=()
	if [ -n "$namespace" ]; then
		scope=(-n "$namespace")
	fi
	kube auth can-i "$verb" "$resource" "${scope[@]}" \
		--as="system:serviceaccount:${NAMESPACE}:${service_account}" >/dev/null 2>&1
}
expect_allow() {
	local service_account="$1" verb="$2" resource="$3" namespace="${4:-}"
	if ! can_i "$service_account" "$verb" "$resource" "$namespace"; then
		echo "expected ${service_account} to be allowed to ${verb} ${resource}${namespace:+/${namespace}}" >&2
		failures=$((failures + 1))
	fi
}
expect_deny() {
	local service_account="$1" verb="$2" resource="$3" namespace="${4:-}"
	if can_i "$service_account" "$verb" "$resource" "$namespace"; then
		echo "expected ${service_account} to be denied ${verb} ${resource}${namespace:+/${namespace}}" >&2
		failures=$((failures + 1))
	fi
}

# Discovery is granted to every authenticated identity by the cluster.
expect_allow "$KUBERNETES_SA" get pods "$WORKLOAD_NAMESPACE"
expect_allow "$KUBERNETES_SA" list pods "$WORKLOAD_NAMESPACE"
expect_allow "$KUBERNETES_SA" get pods/log "$WORKLOAD_NAMESPACE"
expect_allow "$KUBERNETES_SA" get deployments.apps "$WORKLOAD_NAMESPACE"
expect_allow "$KUBERNETES_SA" list events "$WORKLOAD_NAMESPACE"
expect_allow "$KUBERNETES_SA" list namespaces
expect_allow "$KUBERNETES_SA" get nodes

# Secrets, execution, port forwarding, token issuance and every mutation are out.
expect_deny "$KUBERNETES_SA" get secrets "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" list secrets "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" create pods/exec "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" create pods/attach "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" create pods/portforward "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" create serviceaccounts/token "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" create pods "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" update deployments.apps "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" patch deployments.apps "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" delete pods "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" update deployments.apps/scale "$WORKLOAD_NAMESPACE"
expect_deny "$KUBERNETES_SA" delete events "$WORKLOAD_NAMESPACE"

# The server and agent identities hold no cluster permission.
for service_account in "$SERVER_SA" "$AGENT_SA"; do
	expect_deny "$service_account" get pods "$WORKLOAD_NAMESPACE"
	expect_deny "$service_account" list secrets "$WORKLOAD_NAMESPACE"
	expect_deny "$service_account" create pods "$WORKLOAD_NAMESPACE"
	expect_deny "$service_account" get nodes ""
done

# Only the Kubernetes integration automounts a service account token.
for deployment in "$RELEASE-server:$SERVER_SA:false" "$RELEASE-agent:$AGENT_SA:false" \
	"$RELEASE-kubernetes-mcp:$KUBERNETES_SA:true"; do
	name="${deployment%%:*}"
	rest="${deployment#*:}"
	service_account="${rest%%:*}"
	expected="${rest##*:}"
	automount=$(kube -n "$NAMESPACE" get deployment "$name" \
		-o jsonpath='{.spec.template.spec.automountServiceAccountToken}' 2>/dev/null || true)
	if [ -z "$automount" ]; then
		automount=false
	fi
	if [ "$automount" != "$expected" ]; then
		echo "deployment ${name}: automountServiceAccountToken is ${automount}, want ${expected}" >&2
		failures=$((failures + 1))
	fi
done

if [ "$failures" -ne 0 ]; then
	die "${failures} authorization check(s) failed."
fi
echo "authorization: the read grant holds and no runtime identity can mutate, execute, or read Secrets."

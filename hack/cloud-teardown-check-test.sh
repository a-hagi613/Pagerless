#!/usr/bin/env bash
# Proves that the teardown check reports an EBS volume even when the Resource
# Groups tagging index does not return it, and that a failed AWS query fails the
# check instead of passing it.
#
# The real check script runs against a fake `aws` on PATH, so this needs no
# credentials and never touches an account. `make cloud-check` and the Terraform
# CI job run it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check="${here}/cloud-teardown-check.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
bin="${work}/bin"
mkdir -p "$bin"
cat >"${bin}/aws" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_STATE:?}"
if [ -f "${state}/query-failure" ]; then
	echo "An error occurred (AccessDenied) when calling the DescribeVolumes operation" >&2
	exit 254
fi
case "${1:-} ${2:-}" in
"resourcegroupstaggingapi get-resources")
	cat "${state}/tagged.txt" 2>/dev/null || true
	;;
"ec2 describe-volumes")
	if printf '%s\n' "$*" | grep -q -- 'tag-key'; then
		cat "${state}/csi-volumes.txt" 2>/dev/null || true
	else
		cat "${state}/tagged-volumes.txt" 2>/dev/null || true
	fi
	;;
"kms describe-key")
	cat "${state}/kms-state.txt" 2>/dev/null || true
	;;
*)
	echo "unexpected aws invocation: $*" >&2
	exit 1
	;;
esac
FAKE
chmod +x "${bin}/aws"

failures=0
fail() {
	echo "FAIL ${1}" >&2
	shift
	sed 's/^/    /' "$@" >&2
	failures=$((failures + 1))
}

new_state() {
	local state="${work}/${1}"
	mkdir -p "$state"
	printf '%s\n' "$state"
}

# check_case runs the real check against one state directory and records whether
# it exits with the expected status.
check_case() {
	local name="$1" want="$2" state="$3" status=0
	PATH="${bin}:${PATH}" FAKE_STATE="${state}" TEARDOWN_CHECK_ATTEMPTS=1 TEARDOWN_CHECK_INTERVAL_SECONDS=0 \
		bash "$check" >"${state}/stdout" 2>"${state}/stderr" || status=$?
	if [ "$status" -ne "$want" ]; then
		fail "${name}: exit ${status}, want ${want}" "${state}/stdout" "${state}/stderr"
	fi
}

clean="$(new_state clean)"
check_case "a clean account passes" 0 "$clean"
grep -q 'The teardown check passed' "${clean}/stdout" ||
	fail "the clean case did not report a pass" "${clean}/stdout"

# The tag index is what a volume escaped through. The direct EC2 query must catch
# it even when the index says nothing is left.
tagged_volume="$(new_state tagged-volume)"
printf 'vol-0tagged\n' >"${tagged_volume}/tagged-volumes.txt"
check_case "a tagged volume fails the check without the tagging index" 1 "$tagged_volume"
grep -q 'volume/vol-0tagged' "${tagged_volume}/stderr" ||
	fail "the tagged volume is not named in the failure" "${tagged_volume}/stderr"

csi_volume="$(new_state csi-volume)"
printf 'vol-0csi\n' >"${csi_volume}/csi-volumes.txt"
check_case "an untagged CSI volume fails the check" 1 "$csi_volume"
grep -q 'volume/vol-0csi' "${csi_volume}/stderr" ||
	fail "the CSI volume is not named in the failure" "${csi_volume}/stderr"

failed_query="$(new_state failed-query)"
: >"${failed_query}/query-failure"
check_case "a failed AWS query fails the check" 1 "$failed_query"
grep -q 'AWS query failed' "${failed_query}/stderr" ||
	fail "the failed query is not reported" "${failed_query}/stderr"

pending="$(new_state pending-key)"
printf 'arn:aws:kms:us-east-1:207567775100:key/1234\n' >"${pending}/tagged.txt"
printf 'PendingDeletion\n' >"${pending}/kms-state.txt"
check_case "a key waiting for its deletion window passes" 0 "$pending"
grep -q 'waiting for deletion' "${pending}/stdout" ||
	fail "the pending key is not reported" "${pending}/stdout"

if [ "$failures" -ne 0 ]; then
	echo "${failures} teardown-check test(s) failed." >&2
	exit 1
fi
echo "The teardown-check tests passed."

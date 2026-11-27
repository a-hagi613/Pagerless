#!/usr/bin/env bash
# Exercises the release guards without a cluster, a registry, or a network.
#
# The release job refuses a legacy tag, a version that disagrees with the tree,
# a legacy image coordinate, and a tag whose trigger is no longer the fresh
# pagerless-v prefix. This script proves those refusals before a tag is pushed.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

root="$REPO_ROOT"
chart_version="$(awk '$1 == "version:" { print $2; exit }' "$root/charts/pagerless/Chart.yaml")"
work="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-release-guards.XXXXXXXX")"
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

failures=0
pass() { printf 'ok: %s\n' "$*"; }
fail() {
	printf 'not ok: %s\n' "$*" >&2
	failures=$((failures + 1))
}

expect_ok() {
	local name="$1"
	shift
	if "$@" >"$work/out" 2>"$work/err"; then
		pass "$name"
	else
		fail "$name"
		cat "$work/out" "$work/err" >&2
	fi
}

expect_fail() {
	local name="$1"
	shift
	if "$@" >"$work/out" 2>"$work/err"; then
		fail "$name (unexpectedly passed)"
	else
		pass "$name"
	fi
}

# A legacy tag is immutable: the old v1.0.0 and v2.0.0 releases, their assets,
# their images, and their installers must never be targeted again.
expect_fail "legacy v1.0.0 tag is refused" \
	bash "$root/scripts/release/check-release-target.sh" \
	--tag v1.0.0 --root "$root" --offline
expect_fail "legacy v2.0.0 tag is refused" \
	bash "$root/scripts/release/check-release-target.sh" \
	--tag v2.0.0 --root "$root" --offline

# The only fresh tag for this release is the prefixed one.
expect_ok "pagerless-v${chart_version} is accepted" \
	bash "$root/scripts/release/check-release-target.sh" \
	--tag "pagerless-v${chart_version}" --root "$root" --offline
expect_fail "a version that disagrees with the chart is refused" \
	bash "$root/scripts/release/check-release-target.sh" \
	--tag "pagerless-v${chart_version}" --version 9.9.9 --root "$root" --offline

# The installed images and the built images come from one source of truth.
expect_ok "the chart version wiring is consistent" \
	bash "$root/scripts/release/check-versions.sh" --version "$chart_version"

cp -R "$root/charts/pagerless" "$work/chart"
sed -i '0,/repository: ghcr.io\/a-hagi613\/pagerless\/core\/server/s//repository: ghcr.io\/a-hagi613\/pagerless-server/' \
	"$work/chart/values.yaml"
expect_fail "a legacy image repository is refused" \
	bash "$root/scripts/release/check-versions.sh" --version "$chart_version" --chart-dir "$work/chart"

cp -R "$root/charts/pagerless" "$work/chart-tag"
sed -i '0,/^  tag: ""/s//  tag: 9.9.9/' "$work/chart-tag/values.yaml"
expect_fail "an image tag that disagrees with the release is refused" \
	bash "$root/scripts/release/check-versions.sh" --version "$chart_version" --chart-dir "$work/chart-tag"

# The release workflow listens to the fresh tag only. A legacy v tag must not
# start a publication.
if grep -Eq 'tags: *\[ *"v\*\.\*\.\*"' "$root/.github/workflows/release.yml"; then
	fail "the release workflow still triggers on legacy v tags"
else
	pass "the release workflow does not trigger on legacy v tags"
fi
grep -q 'pagerless-v\*\.\*\.\*' "$root/.github/workflows/release.yml" ||
	fail "the release workflow does not trigger on the fresh pagerless-v tag"

# The anonymous access job reads the tag and version from the validate job.
# A transitive dependency does not expose those outputs, so validate must be
# named directly in needs: (the 1.0.0 run failed with an empty version).
access_block="$(awk '
	/^  access:/ { in_access = 1 }
	in_access && /^  [a-z][a-z-]*:/ && !/^  access:/ { exit }
	in_access { print }
' "$root/.github/workflows/release.yml")"
if printf '%s\n' "$access_block" | grep -Eq '^    needs:.*(^|[^a-z-])validate([^a-z-]|$)'; then
	pass "the anonymous access job depends on validate directly"
else
	fail "the anonymous access job does not depend on validate directly"
fi

if [ "$failures" -ne 0 ]; then
	die "$failures release guard check(s) failed."
fi
echo "release guards: every immutable-target, version, and trigger check holds."

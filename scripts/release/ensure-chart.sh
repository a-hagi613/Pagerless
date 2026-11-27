#!/usr/bin/env bash
# Publishes the release chart, or proves that the published tag is byte-for-byte
# the chart the verify job built.
#
# The SHA-256 of the verified chart tarball is recorded beside the digest
# markers on the draft release. A resumed publication compares the live chart
# with that record and with the local tarball before it skips the push, so an
# interrupted first publication never overwrites a different chart.
#
# Usage: ensure-chart.sh --version VERSION --chart-file FILE --sha-file FILE
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions

version=""
chart_file=""
sha_file=""

usage() {
	cat >&2 <<'EOF'
Usage: ensure-chart.sh --version VERSION --chart-file FILE --sha-file FILE

  --version VERSION   The release version without a prefix.
  --chart-file FILE   The verified pagerless-<version>.tgz.
  --sha-file FILE     Where the chart tarball SHA-256 is recorded.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		version="${2:?--version needs a value}"
		shift 2
		;;
	--chart-file)
		chart_file="${2:?--chart-file needs a path}"
		shift 2
		;;
	--sha-file)
		sha_file="${2:?--sha-file needs a path}"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage
		die "unknown argument $1"
		;;
	esac
done

if [ -z "$version" ] || [ -z "$chart_file" ] || [ -z "$sha_file" ]; then
	usage
	die "--version, --chart-file, and --sha-file are required"
fi
[ -f "$chart_file" ] || die "$chart_file does not exist."

chart_ref="${PAGERLESS_CHART_OCI}/${PAGERLESS_CHART_NAME}"
local_sha="$(sha256sum "$chart_file" | awk '{print $1}')"
mkdir -p "$(dirname "$sha_file")"

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-ensure-chart.XXXXXXXX")"
trap 'rm -rf "$state"' EXIT

if helm show chart "$chart_ref" --version "$version" >/dev/null 2>"$state/err"; then
	[ -f "$sha_file" ] ||
		die "${chart_ref}:${version} already exists but no chart digest was recorded; refusing to overwrite it. Before the first public announcement an owner must remove the partial tag."
	recorded="$(cat "$sha_file")"
	[ -n "$recorded" ] || die "$sha_file is empty; refusing to overwrite ${chart_ref}:${version}."
	helm pull "$chart_ref" --version "$version" --destination "$state" >/dev/null
	remote_sha="$(sha256sum "$state/pagerless-${version}.tgz" | awk '{print $1}')"
	[ "$remote_sha" = "$recorded" ] ||
		die "${chart_ref}:${version} is $remote_sha, not the recorded $recorded; refusing to overwrite a published chart."
	[ "$local_sha" = "$recorded" ] ||
		die "$chart_file is $local_sha, not the recorded $recorded; refusing to publish a different chart under a recorded tag."
	echo "${chart_ref}:${version} is already published as $remote_sha; skipping the push." >&2
	exit 0
fi
if ! grep -qiE 'not found|manifest unknown|no such manifest|name unknown|404' "$state/err"; then
	die "cannot prove that ${chart_ref}:${version} is unpublished: $(tr '\n' ' ' <"$state/err")"
fi

echo "publishing ${chart_ref}:${version}" >&2
helm push "$chart_file" "$PAGERLESS_CHART_OCI" >"$state/push.log"
printf '%s\n' "$local_sha" >"$sha_file"
cat "$state/push.log"
echo "chart published with tarball sha256 $local_sha"

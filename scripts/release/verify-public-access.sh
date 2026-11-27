#!/usr/bin/env bash
# Checks the published release the way an anonymous operator reaches it: no
# GitHub token, no registry login.
#
# The GitHub Release assets must download with plain curl. The two images and
# the chart must be readable through an anonymous GHCR token. When GHCR still
# answers 401/403, the release is published correctly but the package
# visibility is an owner action; this script reports that instead of hiding it.
#
# Exit status: 0 every coordinate is public, 2 an owner action is required,
# 1 a request itself failed.
#
# Usage: verify-public-access.sh --tag TAG --version VERSION [--repo OWNER/NAME]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions

tag=""
version=""
repo="a-hagi613/pagerless"

usage() {
	cat >&2 <<'EOF'
Usage: verify-public-access.sh --tag TAG --version VERSION [--repo OWNER/NAME]

  --tag TAG          The immutable release tag, such as pagerless-v1.0.0.
  --version VERSION  The release version without a prefix.
  --repo OWNER/NAME  The GitHub repository. Default: a-hagi613/pagerless
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--tag)
		tag="${2:?--tag needs a value}"
		shift 2
		;;
	--version)
		version="${2:?--version needs a value}"
		shift 2
		;;
	--repo)
		repo="${2:?--repo needs OWNER/NAME}"
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

[ -n "$tag" ] || {
	usage
	die "--tag is required"
}
[ -n "$version" ] || {
	usage
	die "--version is required"
}
require_commands curl jq

summary="${GITHUB_STEP_SUMMARY:-/dev/null}"
rows=()
action_required=0
request_failed=0

record() {
	local name="$1" status="$2" detail="$3"
	rows+=("| $name | $status | $detail |")
	case "$status" in
	"" | public) ;;
	owner-action)
		action_required=1
		;;
	*)
		request_failed=1
		;;
	esac
}

github_asset() {
	local name="$1" url code
	url="https://github.com/${repo}/releases/download/${tag}/${name}"
	code="$(curl --silent --show-error --location --max-time 60 \
		--output /dev/null --write-out '%{http_code}' "$url" || true)"
	case "$code" in
	200) record "$name" public "HTTP 200 without a token" ;;
	404) record "$name" missing "HTTP 404" ;;
	*) record "$name" request-failed "HTTP ${code:-none}" ;;
	esac
}

ghcr_manifest() {
	local name="$1" repository="$2" reference="$3"
	local token code
	token="$(curl --fail --silent --show-error --max-time 30 \
		"https://ghcr.io/token?service=ghcr.io&scope=repository:${repository}:pull" |
		jq -r '.token // empty' || true)"
	if [ -z "$token" ]; then
		record "$name" request-failed "no anonymous GHCR token"
		return
	fi
	code="$(curl --silent --show-error --max-time 30 \
		--output /dev/null --write-out '%{http_code}' \
		-H "Authorization: Bearer ${token}" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json' \
		"https://ghcr.io/v2/${repository}/manifests/${reference}" || true)"
	case "$code" in
	200) record "$name" public "anonymous manifest read" ;;
	401 | 403) record "$name" owner-action "privately visible (HTTP $code)" ;;
	404) record "$name" missing "HTTP 404" ;;
	*) record "$name" request-failed "HTTP ${code:-none}" ;;
	esac
}

for archive in \
	"kubectl-page_${version}_linux_amd64.tar.gz" \
	"kubectl-page_${version}_linux_arm64.tar.gz" \
	"kubectl-page_${version}_darwin_amd64.tar.gz" \
	"kubectl-page_${version}_darwin_arm64.tar.gz" \
	"checksums.txt" \
	"page.yaml" \
	"pagerless-${version}.tgz" \
	"pagerless-${version}.tgz.sha256"; do
	github_asset "$archive"
done

ghcr_manifest "server image" "${PAGERLESS_SERVER_REPOSITORY#ghcr.io/}" "$version"
ghcr_manifest "agent image" "${PAGERLESS_AGENT_REPOSITORY#ghcr.io/}" "$version"
ghcr_manifest "chart" "${PAGERLESS_CHART_OCI#oci://ghcr.io/}/${PAGERLESS_CHART_NAME}" "$version"

{
	echo "### Anonymous access to ${tag}"
	echo
	echo "| artifact | access | detail |"
	echo "| --- | --- | --- |"
	printf '%s\n' "${rows[@]}"
	if [ "$action_required" -eq 1 ]; then
		echo
		echo "**Owner action required:** make the three GHCR packages public in"
		echo "the repository package settings, or publish the chart fallback from"
		echo "the GitHub Release, which is already downloadable without a token."
	fi
} | tee -a "$summary"

if [ "$request_failed" -ne 0 ]; then
	die "one or more anonymous access checks could not be completed."
fi
if [ "$action_required" -ne 0 ]; then
	echo "anonymous access: an owner must make the GHCR packages public." >&2
	exit 2
fi
echo "anonymous access: the release, images, and chart are public."

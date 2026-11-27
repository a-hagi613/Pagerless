#!/usr/bin/env bash
# Refuses a release target that is not the fresh, immutable 1.0 distribution.
#
# The legacy v1.0.0 and v2.0.0 tags, releases, charts, and images are frozen.
# This guard fails before any publication when the tag is legacy, when the tag
# and the tree disagree, when the chart points at a retired image coordinate,
# or when a release already exists for the tag. It never creates, moves, or
# deletes a tag or a release.
#
# Usage:
#   check-release-target.sh --tag TAG [--version VERSION] [--root DIR]
#     [--repo OWNER/NAME] [--require-tag] [--offline]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

tag=""
version=""
root="$REPO_ROOT"
repo="a-hagi613/pagerless"
require_tag=false
offline=false

usage() {
	cat >&2 <<'EOF'
Usage: check-release-target.sh --tag TAG [options]

  --tag TAG          The immutable release tag, such as pagerless-v1.0.0.
  --version VERSION  The version without a prefix; must equal the tag's version.
  --root DIR         The checkout to inspect. Default: this repository.
  --repo OWNER/NAME  The GitHub repository. Default: a-hagi613/pagerless
  --require-tag      Fail unless the tag exists and points at HEAD.
  --offline          Do not ask GitHub whether the release already exists.
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
	--root)
		root="${2:?--root needs a directory}"
		shift 2
		;;
	--repo)
		repo="${2:?--repo needs OWNER/NAME}"
		shift 2
		;;
	--require-tag)
		require_tag=true
		shift
		;;
	--offline)
		offline=true
		shift
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
[ -d "$root" ] || die "$root is not a directory."

# A v-prefixed tag belongs to the retired operator. Publishing one would
# overwrite a legacy artifact, so reject every non-prefixed tag by name.
case "$tag" in
v*)
	die "refusing $tag: legacy v-prefixed tags are immutable and are never published again."
	;;
esac

# shellcheck disable=SC2034 # BASH_REMATCH is read through the capture below.
if [[ ! "$tag" =~ ^pagerless-v([0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?)$ ]]; then
	die "refusing $tag: the fresh release tag must look like pagerless-v<major>.<minor>.<patch>."
fi
tag_version="${BASH_REMATCH[1]}"
if [ -n "$version" ] && [ "$version" != "$tag_version" ]; then
	die "refusing $tag: the tag names $tag_version but the workflow asked for $version."
fi
version="$tag_version"

[ -f "$root/hack/versions.env" ] || die "$root/hack/versions.env does not exist."
set -a
# shellcheck source=hack/versions.env
. "$root/hack/versions.env"
set +a

[ "${PAGERLESS_RELEASE_TAG_PREFIX:-}" = "pagerless-v" ] ||
	die "hack/versions.env does not pin the pagerless-v tag prefix."

chart="$root/charts/pagerless/Chart.yaml"
values="$root/charts/pagerless/values.yaml"
[ -f "$chart" ] || die "$chart does not exist."
[ -f "$values" ] || die "$values does not exist."

chart_version="$(awk '$1 == "version:" { print $2; exit }' "$chart")"
app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' "$chart")"
[ "$chart_version" = "$version" ] ||
	die "$chart has version $chart_version, not $version."
[ "$app_version" = "$version" ] ||
	die "$chart has appVersion $app_version, not $version."

image_repository() {
	awk -v key="$1" '
		$0 == key ":" { inimage = 1; next }
		inimage && /^[^ ]/ { inimage = 0 }
		inimage && /^  repository:/ { print $2; exit }
	' "$values"
}

[ "$(image_repository image)" = "$PAGERLESS_SERVER_REPOSITORY" ] ||
	die "the chart server image is not $PAGERLESS_SERVER_REPOSITORY."
[ "$(image_repository agentImage)" = "$PAGERLESS_AGENT_REPOSITORY" ] ||
	die "the chart agent image is not $PAGERLESS_AGENT_REPOSITORY."

# Legacy coordinates must not survive anywhere in the tree, not just in the
# chart: a stale workflow, script, or document could publish over them.
if git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
	legacy='ghcr\.io/a-hagi613/(pagerless-server|pagerless-agent)|oci://ghcr\.io/a-hagi613/charts'
	if git -C "$root" grep -n -I -E "$legacy" -- . \
		':!scripts/release/check-release-target.sh' \
		':!scripts/release/test-guards.sh'; then
		die "legacy distribution coordinates remain in the tree; remove them before publishing."
	fi
fi

if [ "$require_tag" = true ]; then
	tag_commit="$(git -C "$root" rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)" ||
		die "the tag $tag does not exist in $root."
	head_commit="$(git -C "$root" rev-parse HEAD)"
	[ "$tag_commit" = "$head_commit" ] ||
		die "the tag $tag points at $tag_commit, not at HEAD $head_commit."
fi

if [ "$offline" = false ]; then
	require_commands gh
	state_file="$(mktemp "${TMPDIR:-/tmp}/pagerless-release-state.XXXXXXXX")"
	trap 'rm -f "$state_file"' EXIT
	if draft="$(
		GH_TOKEN="${GH_TOKEN:-}" gh release view "$tag" --repo "$repo" \
			--json isDraft --jq '.isDraft' 2>"$state_file"
	)"; then
		case "$draft" in
		true)
			echo "resuming the unfinished draft release for $tag; a draft is not public." >&2
			;;
		false)
			die "$repo already published a release for $tag; an immutable release is never modified."
			;;
		*)
			die "cannot tell whether $repo has a release for $tag; refusing to publish."
			;;
		esac
	else
		grep -q 'release not found' "$state_file" || {
			cat "$state_file" >&2
			die "cannot read the release state for $tag; refusing to publish."
		}
		# No release exists yet, which is the normal first run.
	fi
fi

echo "release target $tag (v$version) is fresh, consistent, and does not reuse a legacy coordinate."

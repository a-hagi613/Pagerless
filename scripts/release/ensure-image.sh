#!/usr/bin/env bash
# Publishes one multi-arch release image, or proves that the published tag is
# the same digest a previous attempt recorded.
#
# A first publication can be interrupted between the two images, the chart, and
# the GitHub Release. Re-running the workflow must resume without overwriting a
# published tag. The digest marker written here is uploaded to the draft
# release immediately, so a later attempt compares the live manifest digest
# with the recorded one and skips the push only when they are identical.
#
# Usage: ensure-image.sh --name server|agent --version VERSION --revision SHA
#          --digest-file FILE [--platforms linux/amd64,linux/arm64]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions

name=""
version=""
revision=""
digest_file=""
platforms="linux/amd64,linux/arm64"

usage() {
	cat >&2 <<'EOF'
Usage: ensure-image.sh --name server|agent --version VERSION --revision SHA --digest-file FILE

  --name NAME         server or agent.
  --version VERSION   The release version without a prefix.
  --revision SHA      The exact source commit the image must carry.
  --digest-file FILE  Where the manifest-list digest is recorded.
  --platforms LIST    Default: linux/amd64,linux/arm64
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--name)
		name="${2:?--name needs server or agent}"
		shift 2
		;;
	--version)
		version="${2:?--version needs a value}"
		shift 2
		;;
	--revision)
		revision="${2:?--revision needs a commit}"
		shift 2
		;;
	--digest-file)
		digest_file="${2:?--digest-file needs a path}"
		shift 2
		;;
	--platforms)
		platforms="${2:?--platforms needs a list}"
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

if [ -z "$name" ] || [ -z "$version" ] || [ -z "$revision" ] || [ -z "$digest_file" ]; then
	usage
	die "--name, --version, --revision, and --digest-file are required"
fi
case "$name" in
server)
	image="$PAGERLESS_SERVER_IMAGE"
	dockerfile="Dockerfile.pagerless-server"
	;;
agent)
	image="$PAGERLESS_AGENT_IMAGE"
	dockerfile="Dockerfile.pagerless-agent"
	;;
*)
	die "--name must be server or agent."
	;;
esac

require_commands docker jq
mkdir -p "$(dirname "$digest_file")"

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-ensure-image.XXXXXXXX")"
trap 'rm -rf "$state"' EXIT

remote_digest() {
	docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}'
}

if remote_digest "$image" >"$state/digest" 2>"$state/err"; then
	live="$(cat "$state/digest")"
	[ -f "$digest_file" ] ||
		die "$image already exists but no digest was recorded; refusing to overwrite it. Before the first public announcement an owner must remove the partial tag."
	recorded="$(cat "$digest_file")"
	[ "$live" = "$recorded" ] ||
		die "$image now points at $live, not the recorded $recorded; refusing to overwrite a published image."
	echo "$image is already published at $live; skipping the push." >&2
	exit 0
fi
if ! grep -qiE 'not found|manifest unknown|no such manifest|name unknown' "$state/err"; then
	die "cannot prove that $image is unpublished: $(tr '\n' ' ' <"$state/err")"
fi

echo "publishing $image for ${platforms} from ${revision}" >&2
docker buildx build --push --platform "$platforms" \
	--file "$dockerfile" \
	--build-arg "VERSION=${version}" --build-arg "VCS_REF=${revision}" \
	--label "org.opencontainers.image.version=${version}" \
	--label "org.opencontainers.image.revision=${revision}" \
	--metadata-file "$state/meta.json" \
	--tag "$image" "$REPO_ROOT"

digest="$(jq -er '."containerimage.digest"' "$state/meta.json")"
printf '%s\n' "$digest" >"$digest_file"
echo "$image published at $digest"

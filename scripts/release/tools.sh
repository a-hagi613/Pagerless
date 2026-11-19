#!/usr/bin/env bash
# Downloads the pinned release tools into bin/release-tools and verifies each
# archive against the checksum in hack/versions.env. The tools are the only
# binaries a release needs, and none of them needs a credential to install, so
# the same script prepares a laptop and a CI runner.
#
# Usage: tools.sh [all|goreleaser|syft|krew]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"
load_versions

tools_dir="${RELEASE_TOOLS_DIR:-$REPO_ROOT/bin/release-tools}"

# fetch_tool name version checksum url member installs one binary.
# The member is the path inside the archive, which some projects prefix with ./.
fetch_tool() {
	local name="$1" version="$2" checksum="$3" url="$4" member="$5"
	local target="$tools_dir/$name"
	if [ -x "$target" ]; then
		return
	fi
	require_commands curl sha256sum tar
	local work
	work="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-release-tool.XXXXXXXX")"
	# shellcheck disable=SC2064 # the path must be expanded now.
	trap "rm -rf '$work'" RETURN
	curl --fail --silent --show-error --location "$url" --output "$work/tool.tar.gz"
	echo "$checksum  $work/tool.tar.gz" | sha256sum --check --status ||
		die "the $name archive does not match the pinned checksum for $version."
	mkdir -p "$tools_dir"
	tar -xzf "$work/tool.tar.gz" -C "$work" "$member"
	install -m 0755 "$work/$member" "$target"
}

fetch_goreleaser() {
	fetch_tool goreleaser "$GORELEASER_VERSION" "$GORELEASER_LINUX_AMD64_SHA256" \
		"https://github.com/goreleaser/goreleaser/releases/download/${GORELEASER_VERSION}/goreleaser_Linux_x86_64.tar.gz" \
		goreleaser
}

fetch_syft() {
	fetch_tool syft "$SYFT_VERSION" "$SYFT_LINUX_AMD64_SHA256" \
		"https://github.com/anchore/syft/releases/download/${SYFT_VERSION}/syft_${SYFT_VERSION#v}_linux_amd64.tar.gz" \
		syft
}

fetch_krew() {
	fetch_tool krew "$KREW_VERSION" "$KREW_LINUX_AMD64_SHA256" \
		"https://github.com/kubernetes-sigs/krew/releases/download/${KREW_VERSION}/krew-linux_amd64.tar.gz" \
		./krew-linux_amd64
}

case "${1:-all}" in
all)
	fetch_goreleaser
	fetch_syft
	;;
goreleaser) fetch_goreleaser ;;
syft) fetch_syft ;;
krew) fetch_krew ;;
*)
	die "unknown tool ${1}; use all, goreleaser, syft, or krew."
	;;
esac

echo "release tools are in $tools_dir"

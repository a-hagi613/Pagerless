#!/usr/bin/env bash
# Proves that a published release installs: it downloads every archive from the
# release, checks each one against the release's checksum file, installs the
# rendered Krew plugin manifest, and runs the plugin's version command.
#
# The release workflow runs this script as the release gate, and it runs
# unchanged against any published version on a laptop. Point --base-url at a
# local HTTP server to exercise the same path against a snapshot build.
#
# Usage:
#   verify-artifacts.sh --version VERSION [--base-url URL] [--checksums FILE]
#     [--dist DIR] [--manifest FILE] [--skip-krew]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

version=""
base_url=""
checksums=""
dist=""
manifest=""
install_krew=true

usage() {
	cat >&2 <<'EOF'
Usage: verify-artifacts.sh --version VERSION [options]

  --version VERSION  The published version without the leading v.
  --base-url URL     Where the release assets are served. Default:
                     https://github.com/a-hagi613/pagerless/releases/download/pagerless-vVERSION
  --checksums FILE   Use this checksum file instead of downloading checksums.txt.
  --dist DIR         Use the archives in this directory instead of downloading them.
  --manifest FILE    Install this Krew manifest instead of rendering one from the
                     checksum file.
  --skip-krew        Verify the archives and the version command only.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		version="${2:?--version needs a value}"
		shift 2
		;;
	--base-url)
		base_url="${2:?--base-url needs a URL}"
		shift 2
		;;
	--checksums)
		checksums="${2:?--checksums needs a file}"
		shift 2
		;;
	--dist)
		dist="${2:?--dist needs a directory}"
		shift 2
		;;
	--manifest)
		manifest="${2:?--manifest needs a file}"
		shift 2
		;;
	--skip-krew)
		install_krew=false
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

[ -n "$version" ] || {
	usage
	die "--version is required"
}
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] ||
	die "$version is not a release version without a leading v."
base_url="${base_url:-https://github.com/a-hagi613/pagerless/releases/download/pagerless-v${version}}"
base_url="${base_url%/}"
release_scripts="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

require_commands curl sha256sum tar
work="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-verify-artifacts.XXXXXXXX")"
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

if [ -z "$checksums" ]; then
	checksums="$work/checksums.txt"
	curl --fail --silent --show-error --location "$base_url/checksums.txt" --output "$checksums" ||
		die "could not download $base_url/checksums.txt"
fi
[ -f "$checksums" ] || die "the checksum file $checksums does not exist."

checksum_for() {
	local archive="$1" line
	line="$(awk -v archive="$archive" '$2 == archive { print $1 }' "$checksums")"
	[ -n "$line" ] || die "the checksum file has no line for $archive."
	printf '%s\n' "$line"
}

# fetch verifies and returns the local path of one release asset.
fetch() {
	local asset="$1" sha path
	sha="$(checksum_for "$asset")"
	if [ -n "$dist" ]; then
		path="$dist/$asset"
		[ -f "$path" ] || die "$dist does not contain $asset."
	else
		path="$work/$asset"
		curl --fail --silent --show-error --location "$base_url/$asset" --output "$path" ||
			die "could not download $base_url/$asset"
	fi
	echo "$sha  $path" | sha256sum --check --status ||
		die "$asset does not match the checksum in the release checksum file."
	echo "verified $asset" >&2
	printf '%s\n' "$path"
}

linux_amd64=""
for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os="${pair%/*}"
	arch="${pair#*/}"
	archive="kubectl-page_${version}_${os}_${arch}.tar.gz"
	path="$(fetch "$archive")"
	if [ "$pair" = "linux/amd64" ]; then
		linux_amd64="$path"
	fi
	# Every platform archive must carry the plugin and the license. Only
	# linux/amd64 can be executed on the runner; the other three are proven at
	# the packaging boundary.
	listing="$(tar -tzf "$path")"
	grep -qx 'kubectl-page' <<<"$listing" ||
		die "$archive does not contain kubectl-page."
	grep -qx 'LICENSE' <<<"$listing" ||
		die "$archive does not contain LICENSE."
	# The SBOM is a release artifact too; a missing one means an incomplete release.
	fetch "$archive.sbom.json" >/dev/null
done

extract="$work/extract"
mkdir -p "$extract"
tar -xzf "$linux_amd64" -C "$extract"
plugin="$extract/kubectl-page"
[ -x "$plugin" ] || die "the linux/amd64 archive does not contain an executable kubectl-page."

echo "checking the linux/amd64 binary's build information" >&2
version_output="$("$plugin" version)"
printf '%s\n' "$version_output"
grep -qx "kubectl-page ${version}" <<<"$version_output" ||
	die "the archive's plugin reports a version other than ${version}."
if grep -q '^commit none$' <<<"$version_output"; then
	die "the plugin reports no commit."
fi
if grep -q '^built unknown$' <<<"$version_output"; then
	die "the plugin reports no build date."
fi

if [ "$install_krew" = false ]; then
	echo "archives, checksums, and the plugin version are verified for v${version}"
	exit 0
fi

require_commands kubectl
kubectl krew version >/dev/null 2>&1 ||
	die "install the Krew plugin manager before verifying the plugin artifacts."

if [ -z "$manifest" ]; then
	manifest="$work/page.yaml"
	bash "$release_scripts/render-krew-manifest.sh" \
		--checksums "$checksums" --version "$version" --base-url "$base_url" --output "$manifest"
fi
bash "$release_scripts/lint-krew-manifest.sh" "$manifest" "$checksums"

echo "installing the Krew manifest" >&2
export KREW_ROOT="$work/krew"
export PATH="$KREW_ROOT/bin:$PATH"
kubectl krew install --manifest="$manifest"

installed_output="$(kubectl page version)"
printf '%s\n' "$installed_output"
grep -qx "kubectl-page ${version}" <<<"$installed_output" ||
	die "the installed plugin reports a version other than ${version}."

echo "the v${version} archives, checksums, SBOMs, and Krew manifest are verified"

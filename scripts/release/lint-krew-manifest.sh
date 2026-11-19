#!/usr/bin/env bash
# Lints a Krew plugin manifest.
#
# The lint fails when a rendered manifest contains a ${...} placeholder, when it
# is not the documented Krew API version or not named page, when it does not
# carry exactly the four Linux and macOS selectors on amd64 and arm64, when a URI
# does not name the archive for its selector, when a checksum is not a SHA-256
# value, or when a checksum is absent from the release checksum file. Run it
# against a rendered manifest and the checksum file it was rendered from.
#
# --template lints the repository copy in plugins/page.yaml, which carries
# placeholders until a release renders it. That mode requires the same
# structure, name, selectors, and URIs, but accepts the placeholders and does
# not compare checksums: the release job lints the rendered manifest without
# --template, where both checks apply.
#
# Usage: lint-krew-manifest.sh [--template] MANIFEST [CHECKSUMS]
#
# Without CHECKSUMS the lint checks everything except the checksum values,
# which is what pull-request CI can do before a release exists.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

manifest=""
checksums=""
template=false
if [ "${1:-}" = "--template" ]; then
	template=true
	shift
fi
manifest="${1:-}"
checksums="${2:-}"
if [ -z "$manifest" ]; then
	die "usage: lint-krew-manifest.sh [--template] MANIFEST [CHECKSUMS]"
fi
[ -f "$manifest" ] || die "$manifest does not exist."
if [ -n "$checksums" ]; then
	[ -f "$checksums" ] || die "$checksums does not exist."
fi

failures=0
fail() {
	echo "error: $*" >&2
	failures=$((failures + 1))
}

# 1. No unrendered template placeholder may survive into a rendered manifest.
if [ "$template" = false ] && grep -n '[$]{' "$manifest" >/dev/null; then
	fail "$manifest contains a \${...} placeholder."
fi

# 2. The API version and the plugin name are the documented ones.
grep -qx 'apiVersion: krew.googlecontainertools.github.com/v1alpha2' "$manifest" ||
	fail "$manifest is not apiVersion krew.googlecontainertools.github.com/v1alpha2."
name="$(awk '/^metadata:/ { inmeta = 1; next } inmeta && /^  name:/ { print $2; exit }' "$manifest")"
[ "$name" = "page" ] || fail "$manifest names the plugin ${name:-nothing}, not page."

# 3. Every platform block carries a selector, a checksum, and the archive URI
# that matches its selector; the checksum must come from the checksum file.
platforms="$(
	awk '
	function emit() {
		if (os != "" || arch != "" || sha != "" || uri != "") {
			print os "\t" arch "\t" sha "\t" uri
		}
		os = ""; arch = ""; sha = ""; uri = ""
	}
	/^  platforms:/ { inplatforms = 1; next }
	inplatforms && /^  [^ ]/ { inplatforms = 0 }
	inplatforms && /^    - / { emit() }
	inplatforms && $1 == "uri:" { uri = $2 }
	inplatforms && $1 == "sha256:" { sha = $2 }
	inplatforms && $1 == "os:" { os = $2 }
	inplatforms && $1 == "arch:" { arch = $2 }
	END { emit() }
	' "$manifest"
)"

count="$(printf '%s\n' "$platforms" | grep -c . || true)"
[ "$count" = "4" ] || fail "$manifest has $count platform selectors; four are required."

version="$(awk '/^  version: v/ { print substr($2, 2); exit }' "$manifest")"
[ -n "$version" ] || fail "$manifest has no spec.version."

while IFS=$'\t' read -r os arch sha uri; do
	[ -n "$os" ] || continue
	case "$os/$arch" in
	linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64) ;;
	*) fail "$manifest has an unexpected selector $os/$arch." ;;
	esac
	[ -n "$sha" ] || fail "$manifest has no sha256 for $os/$arch."
	[ -n "$uri" ] || fail "$manifest has no uri for $os/$arch."
	if [ -n "$version" ] && [ -n "$uri" ]; then
		archive="kubectl-page_${version}_${os}_${arch}.tar.gz"
		case "$uri" in
		*/"$archive") ;;
		*) fail "the $os/$arch uri does not end in $archive." ;;
		esac
	fi
	if [ "$template" = false ]; then
		printf '%s' "$sha" | grep -Eqx '[a-f0-9]{64}' ||
			fail "$manifest has a sha256 for $os/$arch that is not a SHA-256 value."
		if [ -n "$checksums" ] && [ -n "$version" ]; then
			archive="kubectl-page_${version}_${os}_${arch}.tar.gz"
			grep -qx -- "$sha  $archive" "$checksums" ||
				fail "the $os/$arch checksum $sha is not in $checksums for $archive."
		fi
	fi
done <<<"$platforms"

# 4. Every selector appears exactly once.
duplicates="$(printf '%s\n' "$platforms" | awk -F'\t' 'NF { print $1 "/" $2 }' | sort | uniq -d)"
[ -z "$duplicates" ] || fail "$manifest repeats the selector $duplicates."

[ "$failures" -eq 0 ] || die "$manifest failed $failures krew manifest check(s)."
if [ "$template" = true ]; then
	echo "$manifest is a valid Krew plugin manifest template."
else
	echo "$manifest is a valid Krew plugin manifest for v${version}."
fi

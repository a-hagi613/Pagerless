#!/usr/bin/env bash
# Fails when the version that identifies a release is not the same in every
# artifact that carries it: the chart's version and appVersion, the image tags
# the chart defaults to, the Krew plugin manifest, the Homebrew formula, and the
# plugin binary.
#
# The release workflow runs this after publishing, before it announces the
# version, so a half-versioned release fails instead of shipping.
#
# Usage:
#   check-versions.sh --version VERSION [--chart-dir DIR] [--manifest FILE]
#     [--formula FILE] [--plugin FILE]
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/../../hack/lib.sh"

version=""
chart_dir="charts/pagerless"
manifest=""
formula=""
plugin=""

usage() {
	cat >&2 <<'EOF'
Usage: check-versions.sh --version VERSION [options]

  --version VERSION  The release version without the leading v.
  --chart-dir DIR    The chart directory. Default: charts/pagerless
  --manifest FILE    The Krew plugin manifest to compare, when one exists.
  --formula FILE     The generated Homebrew formula, when one was generated.
  --plugin FILE      The built kubectl-page binary, when one is available.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		version="${2:?--version needs a value}"
		shift 2
		;;
	--chart-dir)
		chart_dir="${2:?--chart-dir needs a directory}"
		shift 2
		;;
	--manifest)
		manifest="${2:?--manifest needs a file}"
		shift 2
		;;
	--formula)
		formula="${2:?--formula needs a file}"
		shift 2
		;;
	--plugin)
		plugin="${2:?--plugin needs a file}"
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

[ -n "$version" ] || {
	usage
	die "--version is required"
}

failures=0
fail() {
	echo "error: $*" >&2
	failures=$((failures + 1))
}

chart_file="$chart_dir/Chart.yaml"
values_file="$chart_dir/values.yaml"
[ -f "$chart_file" ] || die "$chart_file does not exist."
[ -f "$values_file" ] || die "$values_file does not exist."

chart_version="$(awk '$1 == "version:" { print $2; exit }' "$chart_file")"
app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2; exit }' "$chart_file")"
[ "$chart_version" = "$version" ] ||
	fail "$chart_file has version $chart_version, not $version."
[ "$app_version" = "$version" ] ||
	fail "$chart_file has appVersion $app_version, not $version."

# An empty image tag uses appVersion, so it is identical to the release version
# by construction; an explicit tag must be the release version itself.
while IFS= read -r tag; do
	case "$tag" in
	"" | "$version") ;;
	*) fail "$values_file sets an image tag $tag, not $version." ;;
	esac
done < <(awk '
	/^images:/ { inimages = 1; next }
	inimages && /^[^ ]/ { inimages = 0 }
	inimages && /^    tag:/ { gsub(/"/, "", $2); print $2 }
' "$values_file")

if [ -n "$manifest" ]; then
	[ -f "$manifest" ] || die "$manifest does not exist."
	manifest_version="$(awk '/^  version: v/ { print substr($2, 2); exit }' "$manifest")"
	[ "$manifest_version" = "$version" ] ||
		fail "$manifest has version $manifest_version, not $version."
	while IFS= read -r uri; do
		case "$uri" in
		*/download/v"$version"/*) ;;
		*) fail "$manifest points at $uri, which is not a v$version release asset." ;;
		esac
	done < <(awk '$1 == "uri:" { print $2 }' "$manifest")
fi

if [ -n "$formula" ]; then
	[ -f "$formula" ] || die "$formula does not exist."
	while IFS= read -r uri; do
		case "$uri" in
		*/download/v"$version"/kubectl-page_"$version"_*) ;;
		*) fail "$formula references $uri, which is not a v$version release asset." ;;
		esac
	done < <(awk -F'"' '/^[[:space:]]*url "/ { print $2 }' "$formula")
fi

if [ -n "$plugin" ]; then
	[ -x "$plugin" ] || die "$plugin is not an executable."
	plugin_version="$("$plugin" version | awk 'NR == 1 { print $2 }')"
	[ "$plugin_version" = "$version" ] ||
		fail "$plugin reports version $plugin_version, not $version."
fi

[ "$failures" -eq 0 ] || die "the release version is inconsistent."
echo "every artifact reports v${version}"

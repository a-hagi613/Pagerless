#!/usr/bin/env bash
# Checks every relative link, image path, and in-repository anchor in the
# Markdown files that git tracks.
#
# External targets (http://, https://, mailto:, tel:) are deliberately not
# fetched: the check must never depend on the network, so the gate cannot fail
# because a third-party site is slow, rate-limiting, or briefly down.
#
# Each link is checked as follows:
#
#   - The path part must name a path that `git ls-files` reports, resolved
#     relative to the file that holds the link, or to the repository root when
#     it starts with `/`. A file that exists only in the working tree and is
#     not tracked therefore does not count, so the check matches what a
#     checkout of the commit contains.
#   - A `#anchor` part must match a heading in the target Markdown file, using
#     GitHub's slug rules: lowercase, inline code and emphasis stripped,
#     punctuation removed, spaces replaced by hyphens, and duplicate headings
#     suffixed -1, -2, and so on. An explicit <a name="..."> or <a id="...">
#     anchor matches too.
#   - A link inside a fenced code block is not a link and is skipped, because
#     the documentation shows shell and YAML blocks whose brackets are text.
#
# Every broken link is reported as `path:line → target`, and the script exits
# non-zero when any is broken. The last line is the summary:
# `N files, N links, N broken`.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_commands git realpath

cd "$REPO_ROOT"

state="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-links.XXXXXXXX")"
chmod 700 "$state"
trap 'rm -rf "$state"' EXIT

git ls-files >"$state/tracked"

files=0
links=0
broken=0

# slugify prints the anchor GitHub derives from one heading's text.
slugify() {
	printf '%s\n' "$1" |
		sed -E \
			-e 's/\[([^][]*)\]\([^)]*\)/\1/g' \
			-e 's/<[^>]*>//g' \
			-e 's/`//g' \
			-e 's/\*\*?//g' |
		tr '[:upper:]' '[:lower:]' |
		sed -E \
			-e 's/[^a-z0-9 _-]//g' \
			-e 's/ /-/g'
}

# headings prints the slug of every heading in one file, numbering duplicates
# the way GitHub does.
headings() {
	local -A seen=()
	local heading slug
	while IFS= read -r heading; do
		slug="$(slugify "$heading")"
		[ -n "$slug" ] || continue
		if [ -n "${seen[$slug]+set}" ]; then
			seen[$slug]=$((${seen[$slug]} + 1))
			printf '%s-%d\n' "$slug" "${seen[$slug]}"
		else
			seen[$slug]=0
			printf '%s\n' "$slug"
		fi
	done < <(
		sed -n -E 's/^#{1,6}[[:space:]]+(.*)$/\1/p' "$1" |
			sed -E 's/[[:space:]]+#+[[:space:]]*$//'
	)
}

# explicit prints the anchors a file declares with an HTML <a> tag.
explicit() {
	grep -o -E '<a[[:space:]]+(name|id)="[^"]+"' "$1" 2>/dev/null |
		sed -E 's/.*="([^"]+)"/\1/' || true
}

# anchors_of prints every anchor a file offers, computing it once per run.
declare -A anchors=()
anchors_of() {
	local file="$1"
	if [ -z "${anchors[$file]+set}" ]; then
		anchors[$file]="$({ headings "$file"; explicit "$file"; } | sort -u)"
	fi
	printf '%s\n' "${anchors[$file]}"
}

report() {
	printf '%s:%s → %s (%s)\n' "$1" "$2" "$3" "$4" >&2
}

while IFS= read -r file; do
	files=$((files + 1))
	line=0
	fenced=0
	mapfile -t lines <"$file"
	for text in "${lines[@]}"; do
		line=$((line + 1))
		leading="${text#"${text%%[![:space:]]*}"}"
		if [[ $leading == '```'* || $leading == '~~~'* ]]; then
			fenced=$((1 - fenced))
			continue
		fi
		[ "$fenced" -eq 0 ] || continue
		while IFS= read -r match; do
			[ -n "$match" ] || continue
			target="${match##*](}"
			target="${target%)}"
			# Drop an optional link title: (path "Title") or (path 'Title').
			case "$target" in
			*' "*'*) target="${target%%[[:space:]]*}" ;;
			*" '*"*) target="${target%%[[:space:]]*}" ;;
			esac
			target="${target#"${target%%[![:space:]]*}"}"
			target="${target%"${target##*[![:space:]]}"}"

			links=$((links + 1))
			case "$target" in
			http://* | https://* | mailto:* | tel:* | //*) continue ;;
			esac

			path="${target%%#*}"
			anchor=""
			if [[ $target == *"#"* ]]; then
				anchor="${target#*#}"
			fi

			if [ -z "$path" ]; then
				resolved="$file"
			else
				path="${path#./}"
				if [[ $path == /* ]]; then
					resolved="$(realpath -m -- "$REPO_ROOT$path")"
				else
					resolved="$(realpath -m -- "$(dirname "$file")/$path")"
				fi
				resolved="${resolved#"$REPO_ROOT"/}"
			fi

			if ! grep -Fxq -- "$resolved" "$state/tracked"; then
				report "$file" "$line" "$target" "not a tracked path: $resolved"
				broken=$((broken + 1))
				continue
			fi
			if [ -n "$anchor" ] && [[ $resolved == *.md ]]; then
				if ! anchors_of "$resolved" | grep -Fxq -- "$anchor"; then
					report "$file" "$line" "$target" "no heading or anchor: #$anchor"
					broken=$((broken + 1))
				fi
			fi
		done < <(grep -o -E '!?\[[^][]*\]\([^()]*\)' <<<"$text" || true)
	done
done < <(git ls-files '*.md')

printf '%d files, %d links, %d broken\n' "$files" "$links" "$broken"
[ "$broken" -eq 0 ] || exit 1

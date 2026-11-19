#!/usr/bin/env bash
# Regenerate every Pagerless brand asset from one source image.
#
# Usage:
#   hack/brand-assets.sh [SOURCE]
#
# SOURCE defaults to docs/assets/pagerless-mascot-source.png when it exists,
# otherwise to the committed docs/assets/pagerless-mascot.png, so the pipeline
# is re-runnable on a fresh checkout.
#
# SOURCE must be a PNG with a transparent background. Commit it at no more than
# 1024 px on its long edge: downscale a 4K render before committing it, for
# example with
#   magick render.png -resize '1024x1024>' docs/assets/pagerless-mascot-source.png
# The derivatives below are 512, 256, and 64 px, so nothing is lost by doing so.
#
# Outputs, all deterministic (no timestamps or drift between runs):
#   docs/assets/pagerless-mascot.png        512x512, transparent, 4% margin
#   docs/assets/pagerless-icon.png          256x256, the #0F172A rounded tile
#   dashboard/public/pagerless-mascot.png   256x256, the Portal's copy
#   dashboard/public/favicon.png            64x64, legible at 16 px
#
# The tile radius is 18.75% of its side, the proportion the current icon uses.
# The favicon draws the mark at 88% of its tile instead of the icon's 72%, so
# the green screen survives a 16 px render.
#
# Dependencies: ImageMagick 7 (`magick`), ImageMagick 6 (`convert`), or Python
# Pillow, in that order. Install one of:
#   sudo apt install imagemagick
#   sudo apt install python3-pil
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

MASCOT_SIZE=512
MASCOT_ART=471 # 92% of the canvas, leaving a 4% margin on the long edge.
ICON_SIZE=256
ICON_ART=184 # 72% of the tile.
ICON_RADIUS=48 # 18.75% of the tile, the radius the current icon uses.
PORTAL_MASCOT_SIZE=256
FAVICON_SIZE=64
FAVICON_ART=56 # 88% of the tile, larger than the icon so 16 px stays legible.
FAVICON_RADIUS=12
MAX_SOURCE_EDGE=1024
TILE_COLOR='#0F172A'

usage() {
	cat <<'EOF'
Regenerate every Pagerless brand asset from one source image.

Usage:
  hack/brand-assets.sh [SOURCE]

SOURCE defaults to docs/assets/pagerless-mascot-source.png when present,
otherwise to docs/assets/pagerless-mascot.png. It must be a transparent PNG;
commit it at no more than 1024 px on its long edge.

Outputs:
  docs/assets/pagerless-mascot.png        512x512 transparent mark
  docs/assets/pagerless-icon.png          256x256 rounded navy tile
  dashboard/public/pagerless-mascot.png   256x256 Portal copy
  dashboard/public/favicon.png            64x64 favicon

The script prefers ImageMagick (magick, then convert) and falls back to Python
Pillow. It fails with an install hint when neither is present.
EOF
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac
if [ "$#" -gt 1 ]; then
	usage >&2
	die "takes at most one source image"
fi

source_image="${1:-}"
if [ -z "$source_image" ]; then
	if [ -f docs/assets/pagerless-mascot-source.png ]; then
		source_image=docs/assets/pagerless-mascot-source.png
	else
		source_image=docs/assets/pagerless-mascot.png
	fi
fi
[ -f "$source_image" ] || die "source image not found: $source_image"
[ -r "$source_image" ] || die "source image is not readable: $source_image"

IM=()
IDENTIFY=()
if command -v magick >/dev/null 2>&1; then
	IM=(magick)
	IDENTIFY=(magick identify)
elif command -v convert >/dev/null 2>&1 && command -v identify >/dev/null 2>&1; then
	IM=(convert)
	IDENTIFY=(identify)
elif python3 -c 'import PIL' >/dev/null 2>&1; then
	:
else
	die "install ImageMagick (sudo apt install imagemagick) or Python Pillow (sudo apt install python3-pil)"
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/pagerless-brand.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p docs/assets dashboard/public

source_width=0
source_height=0
if [ "${#IM[@]}" -gt 0 ]; then
	read -r source_width source_height < <("${IDENTIFY[@]}" -format '%w %h' "$source_image")
else
	read -r source_width source_height < <(python3 - "$source_image" <<'PY'
import sys

from PIL import Image

with Image.open(sys.argv[1]) as image:
    print(*image.size)
PY
	)
fi

if [ "$source_width" -gt "$MAX_SOURCE_EDGE" ] || [ "$source_height" -gt "$MAX_SOURCE_EDGE" ]; then
	printf 'warning: %s is %sx%s; commit a copy at most %s px on its long edge, for example:\n' \
		"$source_image" "$source_width" "$source_height" "$MAX_SOURCE_EDGE" >&2
	printf '  magick %s -resize "%sx%s>" docs/assets/pagerless-mascot-source.png\n' \
		"$source_image" "$MAX_SOURCE_EDGE" "$MAX_SOURCE_EDGE" >&2
fi

if [ "${#IM[@]}" -gt 0 ]; then
	opaque="$("${IDENTIFY[@]}" -format '%[opaque]' "$source_image")"
	[ "$opaque" != "True" ] ||
		die "source has no transparent pixels; export the mascot on a transparent background first"
	trimmed="$tmp_dir/trimmed.png"
	"${IM[@]}" "$source_image" -background none -alpha on -trim +repage "PNG32:$trimmed"

	# The documentation mark: trim, fit the long edge to 92%, centre on a square.
	"${IM[@]}" "$trimmed" -resize "${MASCOT_ART}x${MASCOT_ART}" \
		-background none -gravity center -extent "${MASCOT_SIZE}x${MASCOT_SIZE}" \
		-strip -define png:exclude-chunks=date,time \
		"PNG32:docs/assets/pagerless-mascot.png"

	# The Portal's copy is a plain 256 px downscale of that mark.
	"${IM[@]}" docs/assets/pagerless-mascot.png -resize "${PORTAL_MASCOT_SIZE}x${PORTAL_MASCOT_SIZE}" \
		-strip -define png:exclude-chunks=date,time \
		"PNG32:dashboard/public/pagerless-mascot.png"

	# Draw the rounded navy tile, then centre the trimmed mark on it.
	for spec in \
		"${ICON_SIZE} ${ICON_ART} ${ICON_RADIUS} docs/assets/pagerless-icon.png" \
		"${FAVICON_SIZE} ${FAVICON_ART} ${FAVICON_RADIUS} dashboard/public/favicon.png"; do
		read -r tile_size art_size radius output <<<"$spec"
		"${IM[@]}" -size "${tile_size}x${tile_size}" xc:none \
			-fill "$TILE_COLOR" -draw "roundrectangle 0,0,$((tile_size - 1)),$((tile_size - 1)),$radius,$radius" \
			"$tmp_dir/tile-$tile_size.png"
		"${IM[@]}" "$trimmed" -resize "${art_size}x${art_size}" "$tmp_dir/art-$tile_size.png"
		"${IM[@]}" "$tmp_dir/tile-$tile_size.png" "$tmp_dir/art-$tile_size.png" \
			-gravity center -composite -strip -define png:exclude-chunks=date,time \
			"PNG32:$output"
	done
else
	export BRAND_MASCOT_SIZE=$MASCOT_SIZE
	export BRAND_MASCOT_ART=$MASCOT_ART
	export BRAND_ICON_SIZE=$ICON_SIZE
	export BRAND_ICON_ART=$ICON_ART
	export BRAND_ICON_RADIUS=$ICON_RADIUS
	export BRAND_PORTAL_MASCOT_SIZE=$PORTAL_MASCOT_SIZE
	export BRAND_FAVICON_SIZE=$FAVICON_SIZE
	export BRAND_FAVICON_ART=$FAVICON_ART
	export BRAND_FAVICON_RADIUS=$FAVICON_RADIUS
	export BRAND_TILE_COLOR=$TILE_COLOR
	python3 - "$source_image" <<'PY'
import os
import sys

from PIL import Image, ImageDraw

mascot_size = int(os.environ["BRAND_MASCOT_SIZE"])
mascot_art = int(os.environ["BRAND_MASCOT_ART"])
icon_size = int(os.environ["BRAND_ICON_SIZE"])
icon_art = int(os.environ["BRAND_ICON_ART"])
icon_radius = int(os.environ["BRAND_ICON_RADIUS"])
portal_size = int(os.environ["BRAND_PORTAL_MASCOT_SIZE"])
favicon_size = int(os.environ["BRAND_FAVICON_SIZE"])
favicon_art = int(os.environ["BRAND_FAVICON_ART"])
favicon_radius = int(os.environ["BRAND_FAVICON_RADIUS"])
tile_color = os.environ["BRAND_TILE_COLOR"].lstrip("#")

with Image.open(sys.argv[1]) as source:
    image = source.convert("RGBA")

alpha = image.getchannel("A")
if alpha.getextrema()[0] == 255:
    sys.exit("error: source has no transparent pixels; export the mascot on a transparent background first")
bbox = alpha.getbbox()
if bbox is None:
    sys.exit("error: source is fully transparent")
art = image.crop(bbox)


def fit(picture: Image.Image, width: int, height: int) -> Image.Image:
    scale = min(width / picture.width, height / picture.height)
    size = (max(1, round(picture.width * scale)), max(1, round(picture.height * scale)))
    return picture.resize(size, Image.Resampling.LANCZOS)


def save(picture: Image.Image, path: str) -> None:
    picture.save(path, format="PNG", compress_level=9, optimize=False)


def tile(size: int, art_size: int, radius: int) -> Image.Image:
    scale = 4
    mask = Image.new("L", (size * scale, size * scale), 0)
    draw = ImageDraw.Draw(mask)
    draw.rounded_rectangle(
        (0, 0, size * scale - 1, size * scale - 1),
        radius=radius * scale,
        fill=255,
    )
    mask = mask.resize((size, size), Image.Resampling.LANCZOS)
    canvas = Image.new("RGBA", (size, size), tuple(bytes.fromhex(tile_color)) + (255,))
    canvas.putalpha(mask)
    mark = fit(art, art_size, art_size)
    canvas.alpha_composite(mark, ((size - mark.width) // 2, (size - mark.height) // 2))
    return canvas


mascot = Image.new("RGBA", (mascot_size, mascot_size), (0, 0, 0, 0))
mark = fit(art, mascot_art, mascot_art)
mascot.alpha_composite(mark, ((mascot_size - mark.width) // 2, (mascot_size - mark.height) // 2))
save(mascot, "docs/assets/pagerless-mascot.png")
save(mascot.resize((portal_size, portal_size), Image.Resampling.LANCZOS), "dashboard/public/pagerless-mascot.png")
save(tile(icon_size, icon_art, icon_radius), "docs/assets/pagerless-icon.png")
save(tile(favicon_size, favicon_art, favicon_radius), "dashboard/public/favicon.png")
PY
fi

printf 'regenerated the brand assets from %s with %s\n' \
	"$source_image" "${IM[0]:-python3-pillow}"

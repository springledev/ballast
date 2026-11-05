#!/usr/bin/env bash
# Regenerates every raster icon from the SVG sources. The outputs are
# committed, so this only needs to run when the artwork changes, and
# `make web` afterwards copies them into web/dist.
#
# web/public/favicon.svg  the logo, and the master for browser icons
# web/icons/full-bleed.svg  square version for phone home screens
# web/icons/social-preview.svg  the repository's link preview image
#
# Needs ImageMagick 7 built with librsvg.
set -euo pipefail

cd "$(dirname "$0")/.."

# Renders $1 at $2 pixels square, without timestamps so that re-running
# on unchanged artwork leaves the files byte-identical.
render() {
	magick -background none -density 1200 "$1" -resize "$2x$2" -strip \
		-define png:exclude-chunks=date,time "$3"
}

render public/favicon.svg 192 public/icon-192.png
render public/favicon.svg 512 public/icon-512.png
render icons/full-bleed.svg 180 public/apple-touch-icon.png
render icons/full-bleed.svg 512 public/icon-maskable-512.png

# Old browsers, bookmarks and RSS readers still ask for /favicon.ico.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
for size in 16 32 48; do
	render public/favicon.svg "$size" "$tmp/$size.png"
done
magick "$tmp/16.png" "$tmp/32.png" "$tmp/48.png" public/favicon.ico

magick -background none icons/social-preview.svg -strip \
	-define png:exclude-chunks=date,time ../docs/img/social-preview.png

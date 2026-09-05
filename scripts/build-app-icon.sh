#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
: "${1:?Specify the output AppIcon.icns path}"
output=$1
source_image="$PWD/macos/Assets/AppIcon.png"
mkdir -p "$PWD/.cache" "$(dirname "$output")"
stage=$(mktemp -d "$PWD/.cache/app-icon-XXXXXX")
trap 'rm -rf "$stage"' EXIT
iconset="$stage/AppIcon.iconset"
mkdir "$iconset"
# Preserve the generated artwork and alpha while preparing Apple's icon sizes.
for size in 16 32 128 256 512; do
  sips --resampleHeightWidth "$size" "$size" "$source_image" \
    --out "$iconset/icon_${size}x${size}.png" >/dev/null
  retina=$((size * 2))
  sips --resampleHeightWidth "$retina" "$retina" "$source_image" \
    --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil --convert icns --output "$output" "$iconset"

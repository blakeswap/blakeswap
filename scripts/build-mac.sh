#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
python3 scripts/package-version.py >/dev/null
install_path="$PWD/bin/Blakeswap.app"
mkdir -p "$PWD/.cache" "$PWD/bin"
stage_path=$(mktemp -d "$PWD/.cache/mac-build-XXXXXX")
trap 'rm -rf "$stage_path"' EXIT
app_path="$stage_path/Blakeswap.app"
resources="$app_path/Contents/Resources"
mkdir -p "$app_path/Contents/MacOS" "$resources/licenses"
sh scripts/build-app-icon.sh "$resources/AppIcon.icns"
sh scripts/go.sh build -trimpath -o "$resources/blakeswap" ./cmd/blakeswap
swift build --package-path macos --scratch-path .cache/swift-build --cache-path .cache/swift-cache --product Blakeswap -c release
swift_bin=$(swift build --package-path macos --scratch-path .cache/swift-build --show-bin-path -c release)
cp "$swift_bin/Blakeswap" "$app_path/Contents/MacOS/Blakeswap"
for bundle in "$swift_bin"/*.bundle; do
  [ -d "$bundle" ] || continue
  cp -R "$bundle" "$resources/"
done
cp -R docs "$resources/docs"
cp README.md "$resources/docs/README.md"
# Include notices for every resolved Swift dependency and Go module.
python3 scripts/bundle-licenses.py "$resources/licenses"
cat > "$app_path/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>Blakeswap</string>
<key>CFBundleIdentifier</key><string>org.blakeswap.app</string>
<key>CFBundleName</key><string>Blakeswap</string>
<key>CFBundleDisplayName</key><string>Blakeswap</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.2.0</string>
<key>CFBundleVersion</key><string>2</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>LSApplicationCategoryType</key><string>public.app-category.finance</string>
</dict></plist>
PLIST
python3 scripts/package-version.py "$app_path/Contents/Info.plist"
identity=${BLAKESWAP_SIGN_IDENTITY:--}
for executable in "$resources/blakeswap" "$app_path/Contents/MacOS/Blakeswap"; do
  if [ "$identity" = - ]; then codesign --force --sign - "$executable"
  else codesign --force --options runtime --timestamp --sign "$identity" "$executable"; fi
done
if [ "$identity" = - ]; then codesign --force --sign - "$app_path"
else codesign --force --options runtime --timestamp --sign "$identity" "$app_path"; fi
codesign --verify --deep --strict "$app_path"
python3 scripts/verify-bundle.py "$app_path"
# Atomic installation: never overwrite a running Mach-O vnode in place.
if [ -d "$install_path" ]; then
  previous_path=$(mktemp -d "$PWD/.cache/mac-previous-XXXXXX")
  mv "$install_path" "$previous_path/Blakeswap.app"
fi
mv "$app_path" "$install_path"
printf 'Built %s\n' "$install_path"

#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
install_path="$PWD/bin/Blakeswap.app"
mkdir -p "$PWD/.cache" "$PWD/bin"
stage_path=$(mktemp -d "$PWD/.cache/mac-build-XXXXXX")
app_path="$stage_path/Blakeswap.app"
mkdir -p "$app_path/Contents/MacOS" "$app_path/Contents/Resources" "$PWD/.cache/swift"
swiftc -parse-as-library -swift-version 5 -O -target arm64-apple-macosx14.0 \
  -module-cache-path "$PWD/.cache/swift" \
  macos/Blakeswap/Models.swift macos/Blakeswap/UnixRPC.swift macos/Blakeswap/AppModel.swift macos/Blakeswap/BlakeswapApp.swift \
  -o "$app_path/Contents/MacOS/Blakeswap"
cat > "$app_path/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>Blakeswap</string>
<key>CFBundleIdentifier</key><string>org.blakeswap.regtest</string>
<key>CFBundleName</key><string>Blakeswap</string>
<key>CFBundleDisplayName</key><string>Blakeswap</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>0.1.0</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>LSApplicationCategoryType</key><string>public.app-category.finance</string>
</dict></plist>
PLIST
printf '%s\n' "$PWD" > "$app_path/Contents/Resources/workspace.txt"
codesign --force --deep --sign - "$app_path"
if [ -d "$install_path" ]; then
  previous_path=$(mktemp -d "$PWD/.cache/mac-previous-XXXXXX")
  mv "$install_path" "$previous_path/Blakeswap.app"
fi
mv "$app_path" "$install_path"
rmdir "$stage_path"
printf 'Built %s\n' "$install_path"

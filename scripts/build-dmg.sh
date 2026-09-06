#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(python3 scripts/package-version.py)
sh scripts/build-mac.sh
stage=$(mktemp -d "$PWD/.cache/dmg-XXXXXX")
trap 'rm -rf "$stage"' EXIT
cp -R bin/Blakeswap.app "$stage/Blakeswap.app"
ln -s /Applications "$stage/Applications"
arch=$(uname -m)
output="$PWD/bin/Blakeswap-$version-$arch.dmg"
temporary="$PWD/bin/Blakeswap-$version-$arch.build.dmg"
hdiutil create -volname Blakeswap -srcfolder "$stage" -format UDZO -ov "$temporary"
if [ -n "${BLAKESWAP_SIGN_IDENTITY:-}" ]; then codesign --force --timestamp --sign "$BLAKESWAP_SIGN_IDENTITY" "$temporary"; fi
if [ -n "${BLAKESWAP_NOTARY_PROFILE:-}" ]; then
  if [ -z "${BLAKESWAP_SIGN_IDENTITY:-}" ]; then printf 'Notarization requires BLAKESWAP_SIGN_IDENTITY.\n' >&2; exit 1; fi
  xcrun notarytool submit "$temporary" --keychain-profile "$BLAKESWAP_NOTARY_PROFILE" --wait
  xcrun stapler staple "$temporary"
fi
mv "$temporary" "$output"
hdiutil verify "$output"
printf 'Built %s\n' "$output"

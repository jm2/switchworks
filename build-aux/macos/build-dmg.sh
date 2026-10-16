#!/usr/bin/env bash
# Builds Switchworks.app (the menu-bar app with swk inside it) and a .dmg.
# Runs on macOS with the Xcode command-line tools (swiftc, codesign, hdiutil).
#
#   build-aux/macos/build-dmg.sh <version> <swk darwin binary> <output dir>
#
# The app is ad-hoc signed unless MACOS_SIGN_IDENTITY names a "Developer ID
# Application" identity in the keychain; with one, it is signed with the
# hardened runtime, and NOTARY_PROFILE (an `xcrun notarytool
# store-credentials` profile) also notarizes and staples the .dmg.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <version> <swk darwin binary> <output dir>" >&2
  exit 2
fi
version=${1#v}
swk=$2
out=$3
root=$(cd "$(dirname "$0")/../.." && pwd)
arch=$(uname -m) # arm64 on Apple silicon runners
min_macos=13.0

# CFBundleShortVersionString is numeric: 1.2.3 from 1.2.3-rc.1.
bundle_version=${version%%-*}
if [[ ! $bundle_version =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; then
  bundle_version=0.0.0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
app="$work/Switchworks.app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

swiftc -O -target "$arch-apple-macos$min_macos" \
  -framework AppKit -framework ServiceManagement \
  -o "$app/Contents/MacOS/Switchworks" "$root/macos/Switchworks/main.swift"
install -m 0755 "$swk" "$app/Contents/MacOS/swk"
sed "s/@VERSION@/$bundle_version/g" "$root/macos/Switchworks/Info.plist" > "$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist"
cp "$root/data/Switchworks.icns" "$app/Contents/Resources/Switchworks.icns"
cp "$root/LICENSE" "$app/Contents/Resources/LICENSE"

identity=${MACOS_SIGN_IDENTITY:--}
sign_opts=(--force --sign "$identity")
if [[ $identity != "-" ]]; then
  sign_opts+=(--options runtime --timestamp)
fi
# Inside out: the nested binary first, then the bundle.
codesign "${sign_opts[@]}" "$app/Contents/MacOS/swk"
codesign "${sign_opts[@]}" "$app"
codesign --verify --strict --verbose=2 "$app"

mkdir -p "$out"
dmg_arch=aarch64
[[ $arch == x86_64 ]] && dmg_arch=x86_64
dmg="$out/switchworks-macos-$dmg_arch.dmg"
stage="$work/dmg"
mkdir -p "$stage"
cp -R "$app" "$stage/"
ln -s /Applications "$stage/Applications"
rm -f "$dmg"
hdiutil create -volname "Switchworks" -srcfolder "$stage" -ov -format UDZO "$dmg"

if [[ $identity != "-" ]]; then
  codesign --force --sign "$identity" --timestamp "$dmg"
  if [[ -n ${NOTARY_PROFILE:-} ]]; then
    xcrun notarytool submit "$dmg" --keychain-profile "$NOTARY_PROFILE" --wait
    xcrun stapler staple "$dmg"
  fi
fi
echo "$dmg"

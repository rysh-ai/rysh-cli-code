#!/usr/bin/env bash
# Build once on macOS, embed in both Go architectures; no Swift runtime compiler on users' Macs.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [ "$(uname -s)" != Darwin ]; then
  echo 'FaceTime helper must be built on macOS (or supplied by the release artifact).' >&2
  exit 1
fi
build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
for arch in arm64 x86_64; do
  xcrun swiftc -O -target "$arch-apple-macosx12.0" \
    "$root/internal/facetime/native/Core.swift" "$root/internal/facetime/native/BotCore.swift" \
    "$root/internal/facetime/native/BotAudio.swift" "$root/internal/facetime/native/main.swift" \
    -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist \
    -Xlinker "$root/internal/facetime/native/Info.plist" -o "$build_dir/helper-$arch"
done
xcrun lipo -create "$build_dir/helper-arm64" "$build_dir/helper-x86_64" -output "$build_dir/helper"
# A Developer ID can be supplied by the eventual signed macOS release pipeline.
# Ad-hoc signing permits local builds; it does not clear distribution gate U-3.
codesign --force --sign "${RYSH_CONTACTS_SIGN_IDENTITY:--}" --identifier com.rysh.contacts "$build_dir/helper"
mkdir -p "$root/internal/facetime/assets"
cp "$build_dir/helper" "$root/internal/facetime/assets/helper"
chmod 755 "$root/internal/facetime/assets/helper"
xcrun lipo -info "$root/internal/facetime/assets/helper"

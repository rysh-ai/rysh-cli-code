#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
xcrun swiftc "$root/internal/facetime/native/Core.swift" "$root/internal/facetime/native/BotCore.swift" "$root/internal/facetime/native/CoreTests.swift" -o "$build_dir/tests"
"$build_dir/tests"

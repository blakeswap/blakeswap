#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
: "${BLAKESWAP_SWIFT_TEST_ROOT:?Set an isolated prepared desktop-demo data directory}"
if [ -e "$BLAKESWAP_SWIFT_TEST_ROOT/runtime.json" ]; then
  printf 'Quit the app using this test directory first.\n' >&2; exit 1
fi
bin/Blakeswap.app/Contents/Resources/blakeswap desktop --data-dir "$BLAKESWAP_SWIFT_TEST_ROOT" > "$BLAKESWAP_SWIFT_TEST_ROOT/swift-daemon.log" 2>&1 &
daemon_pid=$!
trap 'kill -TERM "$daemon_pid" 2>/dev/null || true; wait "$daemon_pid" || true' EXIT
swift test --package-path macos --scratch-path .cache/swift-build --cache-path .cache/swift-cache -c release --filter DaemonRPCTests

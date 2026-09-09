#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
swift build --package-path macos --scratch-path .cache/swift-build --cache-path .cache/swift-cache --product protoc-gen-swift -c release
swift build --package-path macos --scratch-path .cache/swift-build --cache-path .cache/swift-cache --product protoc-gen-grpc-swift-2 -c release
swift_bin=$(swift build --package-path macos --scratch-path .cache/swift-build --show-bin-path -c release)
mkdir -p macos/Blakeswap/Generated
protoc -I api/proto -I api/third_party \
 --plugin=protoc-gen-swift="$swift_bin/protoc-gen-swift" \
 --plugin=protoc-gen-grpc-swift-2="$swift_bin/protoc-gen-grpc-swift-2" \
 --swift_out=macos/Blakeswap/Generated \
 --grpc-swift-2_out=macos/Blakeswap/Generated \
 --grpc-swift-2_opt=Server=false \
 api/proto/blakeswap/v1/daemon.proto

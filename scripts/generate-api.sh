#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# protoc 3.21.12 or newer; plugin versions and source protos are pinned.
sh scripts/go.sh install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
sh scripts/go.sh install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
sh scripts/go.sh install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.30.0 github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@v2.30.0
export PATH="$PWD/.cache/go-path/bin:$PATH"
protoc -I api/proto -I api/third_party \
 --go_out=api/gen --go_opt=paths=source_relative \
 --go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
 --grpc-gateway_out=api/gen --grpc-gateway_opt=paths=source_relative \
 --openapiv2_out=api --openapiv2_opt=allow_merge=true,merge_file_name=blakeswap,json_names_for_fields=false \
 api/proto/blakeswap/v1/daemon.proto
cp api/blakeswap.swagger.json internal/api/openapi.json

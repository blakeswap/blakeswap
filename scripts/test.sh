#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p .local/test-results
make test-reset test-local-nodes test-packaging
python3 scripts/bootstrap.py
python3 scripts/local.py nodes
sh scripts/go.sh vet ./...
# Unit/race runs skip real-chain cases unless explicitly enabled below.
env -u BLAKESWAP_REGTEST sh scripts/go.sh test -race -count=1 -p=1 ./... fiatjaf.com/nostr/nip44
env -u BLAKESWAP_REGTEST sh scripts/go.sh test ./internal/contract -run '^$' -fuzz '^FuzzParseTransaction$' -fuzztime=10s -parallel=2
env -u BLAKESWAP_REGTEST sh scripts/go.sh test ./internal/transport -run '^$' -fuzz '^FuzzUnwrap$' -fuzztime=10s -parallel=2
# One package at a time: integration tests intentionally manipulate shared nodes.
BLAKESWAP_REGTEST="$PWD" sh scripts/go.sh test -p=1 -count=1 -coverprofile=.local/test-results/coverage.out ./...
BLAKESWAP_TEST_ELECTRUM=1 BLAKESWAP_REGTEST="$PWD" sh scripts/go.sh test -count=1 -run TestRealAsyncSwapRecoveryAndBounties ./internal/daemon
sh scripts/build-mac.sh
printf 'All local verification passed. Coverage: .local/test-results/coverage.out\n'

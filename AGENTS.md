# Repository guidance

## First release: everything is v1

Blakeswap is a greenfield application and protocol that has never been released.
All work is development of the first release. Iterations and new features update
the current design in place; they are not new protocol or product releases.

- Keep application versions at `1.0.0`, and protocol, API, message, wallet-state,
  backup and other application-owned format markers at `1` / `v1`.
- Keep the protobuf package `blakeswap.v1`, HTTP routes `/v1/`, and Nostr
  namespaces `blakeswap-<network>-v1`. Partial fills and other new features belong
  to this same v1 contract.
- Do not introduce v2/v3 packages, release bumps, version negotiation, parallel
  legacy execution paths, or migrations solely to preserve earlier development
  iterations. A future version requires an explicit user request.
- Validate the actual current schema, signatures and invariants, not just a
  version marker. Never relabel incompatible data to make validation pass.
- Preserve existing wallet files, credentials and signed obligations. Document
  any need for a separate development profile or explicit backup/reset; never
  silently delete or reset user data.
- Do not change external versions: Go/Swift dependencies, toolchains, OS targets,
  Bitcoin transaction/header versions, NIP-44, and upstream specifications follow
  their own contracts. Order revisions, settings revisions, transaction fee
  variants, and CI build counters are not application release versions.

## Architecture and correctness

- Go owns wallet keys, signing, protocol advancement and durable state. SwiftUI
  uses the generated private gRPC client; the HTTP gateway shares the same typed
  service. Read `docs/ARCHITECTURE.md`, `docs/PROTOCOL.md` and the relevant feature
  documentation before changing those boundaries.
- Treat `api/proto/blakeswap/v1/daemon.proto` as the API source of truth. Regenerate
  Go/OpenAPI with `sh scripts/generate-api.sh` and Swift with
  `sh scripts/generate-swift.sh`; commit matching generated artifacts.
- Use exact integer amounts and bounded arithmetic. Preserve reviewed user
  consent, immutable accepted terms, per-wallet/network isolation, durable saves
  before broadcast, and recovery evidence across restarts and reorgs.
- Never log or commit mnemonics, private keys, preimages, tokens, node cookies or
  passwords. Keep local fixtures, runtime state, caches and build outputs ignored.

## Validation and documentation

- Use `sh scripts/go.sh` for the pinned Go toolchain. Run focused tests for changed
  behavior, then affected package tests and `vet`; protocol/API/state changes
  warrant the full Go unit suite. Keep race and format checks passing.
- For native changes, run `swift test --package-path macos --scratch-path
  .cache/swift-build --cache-path .cache/swift-cache -c release`. Packaging changes
  require `make test-packaging`; relevant Python checks are exposed in `Makefile`.
- Follow `docs/TESTING.md` for real two-chain validation. Node-mutating suites
  must run serially against isolated regtest fixtures. Never use public funds or
  publish public events as an implicit test step.
- Report skipped or unavailable integration/native checks accurately; unit tests
  are not evidence of real-chain settlement. Keep docs aligned with actual code.
- Do not add `[codex]` to commit messages or PR titles. Follow the repository's
  squash-merge convention when authorized to merge a PR.

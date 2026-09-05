# Local verification record

Verified September 5, 2026 at 15:09 UTC on an Apple Silicon Mac. This records the completed local-regtest goal, not an independent security audit or production certification.

## Running system

- Bitcoin Core 29.1.0, regtest, RPC on loopback port 19443.
- Bitcoin Knots 29.4.1 / 20260508, regtest with Blake2b active from height 1, RPC on loopback port 29443.
- Two persistent Nostr WebSocket relays on loopback ports 7447 and 7448.
- Independent Alice, Bob, and watchtower Go processes with separate encrypted databases and private Unix sockets.
- Native macOS app at `bin/Blakeswap.app`, connected to the final daemon build.

All three daemons reported no current error and no pending outbound messages when checked. Both traders reported the final swap completed. Earlier trades remained completed after application-process restart.

## Fresh trade through the final native build

The native app created Alice's offer, switched to Bob, took the received Nostr offer, and mined each funding and claim phase through its regtest control. The Swaps screen displayed both claims at two confirmations and a zero tower fee. Independent node RPC checks verified that each claim consumes its respective funding outpoint and that neither HTLC remains unspent.

Swap ID:

```
c4e5ccbdd50109896f114750e735df5fa612404f1ea5d71867d4e5d0bfbef631
```

| Asset | Principal | Funding transaction | Claim transaction | Confirmations |
| --- | ---: | --- | --- | ---: |
| Bitcoin Blake2b | 2,000,000 sats | `94d1afec3a656d78afbbf13cdb2e90eb609bd811b33bd9197c0ab10c7409e9a1` | `2b958f0e4a183bb532b6631bab134ee344c6bf256ecaa763d61a3754226c19a3` | 2 |
| Bitcoin | 1,000,000 sats | `d26727b495f801b94064d49457142756a9c4d829ef17d77de82de8aadffa8301` | `145427c89715a677e3e1db5371f4f29b4d1edf720d77fbfa3c132139523b9f34` | 2 |

Chain heights at verification were BTC 341 and BLAKE 406. The demonstration tower quoted 50 basis points, but neither claim used the delayed fallback; no tower bounty was paid. These transaction IDs belong to this local chain history and have no public block-explorer links. A machine-readable public proof is retained in ignored `.local/final-verification.json`.

## Harness results

`sh scripts/test.sh` passed with:

- `go vet` and all package tests under the race detector and checkptr instrumentation.
- The pinned NIP-44 implementation's published test vectors.
- Transaction-parser fuzzing: 347,072 executions in the recorded bounded run.
- Encrypted-envelope fuzzing: 450,159 executions in the recorded bounded run.
- Actual two-chain integration tests, including eight applicable upstream unified-signature vectors, consensus replay asymmetry, asynchronous shutdown/reopen handoffs, owner claims, delayed tower claims, both-party refunds, reverse direction, late-reveal refusal with self-refund, cancellation/reservation races, authenticated receipts, and claim reorg recovery.
- The native macOS build.

The final per-asset fee-status and GUI refinements were followed by passing focused daemon/protocol/transport race tests, another native build, and the fresh GUI trade above. Full harness output remains in ignored `.local/test-results/full-run.log`, with package coverage in `.local/test-results/coverage.out`.

The [invariant matrix](TESTING.md) maps each boundary to its evidence. Tests are finite and do not prove every distributed schedule, deep reorg, fee market, power-loss boundary, or economic attack. The [risk register](RISKS.md) describes those limits and the additional work required before considering real funds.

## Protobuf, external backends and desktop distribution update

Verified September 5, 2026. The original record above remains the initial UI
milestone; the transport and packaging have since changed.

- Real-node RPC and Electrum fixture matrices cover self-claim, offline-maker
  takeover, offline-both refunds, reverse-direction unprotected trade, late taker
  refunds, restart handoffs and claim reorgs. Blake2b reduced-data rules were active.
- Timestamp claim/refund tests passed on both actual regtest nodes at strict
  median-time finality boundaries. Public-network protocol tests cover asymmetric
  heights, freshness/skew, deadline mutations and exact funding/reveal cutoffs.
- Go subprocess tests passed for normal daemon termination and force-killed parent
  cleanup: helper exit, endpoint removal, credential removal and wallet-lock release.
- The DMG built, passed checksum verification, was mounted read-only, and its app
  was copied out and launched. Codesign verification passed. A bundle inventory
  found exactly the UI and Go daemon Mach-O executables, and no nodes/indexers.
- The relocated native app displayed public BTC/Blake2b heights using direct gRPC.
  Subsequent native UI automation failed with “native pipe closed before response,”
  so Settings click-through and a new graphical quit/force-quit sequence could not
  be completed in that session. The equivalent API, process watchdog and native
  transport paths have automated tests; this is not recorded as a passed visual test.
- The **same Swift DaemonRPC client used by the app** executed a full regtest trade
  through its generated protobuf/gRPC bindings (not the HTTP gateway), with an
  external relay and external RPC nodes on ports 21443/31443. Both claims had two
  confirmations and zero tower bounty:

```
swap: e5aea7a3e51aa8d50915b0d2259e16966202660444bc10200ca62af6a69b304d
long Blake2b claim: 8fc859a28bfd970d618f33fe4dd1d7db5b4c2498907ab8561462efdbf6b23e2c
short BTC claim: 7160c4c5d4cf8d63048ede5882ea668f6dd9b2b3da2f4a54f43b3fd84036fd21
```

Public Electrum checks verified real BTC mainnet, Blake2b mainnet, and BTC Testnet4
headers/coinbase inclusion without broadcasting. Public relay REQ/EOSE checks
succeeded for nos.lol, relay.primal.net, and relay.ditto.pub. Availability failures
removed dmrelay.com, relay.damus.io, and relay.nostr.band from the candidate defaults.
No public Blake2b Testnet4 server was verified. No real-money swap, public order,
public encrypted message, Developer ID signature or Apple notarization was part
of this verification.

A repeated all-tests run exposed accumulated watch-only fixture wallets causing
bulk-mining RPC timeouts. The harness now unloads its own observation wallets at
cleanup; node timeouts and protocol assertions were not relaxed.

PR review also identified and corrected historical RPC rescans on reconnect,
public-mempool work scaling, a native network/status publication race, and the
pause HTTP method in the API table. Focused race tests cover incomplete rescan
responses, durable node-wallet readiness, cancellation and lock release, and
relevant preimage observation with a large unrelated mempool. Native XCTest
regressions cover mismatched networks, older Settings revisions and invalidated
profile/Settings generations. Status and Settings now publish together.

The repeated native release test exposed a Swift runtime abort at the generic
`Task.sleep(for:)` specialization, reproduced twice with the same binary. This
matches [Swift issue 86204](https://github.com/swiftlang/swift/issues/86204).
The app poll loop and native tests use the non-generic nanosecond sleep overload;
its cancellation semantics and polling intervals are unchanged. The native test
script also runs the snapshot regressions alongside the real gRPC trade.

With that workaround, all four native release tests passed and a new gRPC trade
completed in 13.65 seconds. Independent RPC checks confirmed both claim inputs
consume their exact funding outpoints, both outputs are spent, both claims have
two confirmations, and the test asserts zero watchtower bounty:

```
swap: 409afdb5372494e11db91007c8139635ed5ba6b19f0a68d042ff199f45a678a4
long Blake2b claim: b5a0c58e58c5e2f67f2e15955f3694276f4c5dd7656b15ee88adc1a204d91611
short BTC claim: 8664251b76c1f79cd0e2039557887097d86f433a041f807f8ceffdaaf4a882c5
```

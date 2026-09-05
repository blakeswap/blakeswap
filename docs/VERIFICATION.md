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

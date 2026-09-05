# Implementation ledger

The entries below record the initial regtest milestone. Current native transport,
packaging and network support are described in [API](API.md),
[Packaging](PACKAGING.md), and [Operations](OPERATIONS.md). The app now owns its
daemon, defaults to public Electrum, and supports all three environments. Public swaps use median-time deadlines and BTC
ancestry checks; the initial short regtest height schedule is retained for tests.


Goal: run BTC and actual Bitcoin Blake2b regtest nodes, local Nostr relay(s), independent Go trader daemons, a keyless delayed-bounty watchtower, and a native macOS GUI. Verify a real atomic trade and adversarial paths. Windows is excluded.

Agreed constraints: one recovery seed with hardened chain and communication branches; Nostr public offers and encrypted persistent mailboxes; asynchronous participation; fixed-size atomic swaps; no DAO, token, treasury or upfront service charge. Watchtower fee is a percentage paid only by a confirmed delayed fallback. The owner can self-claim earlier without that fee. Mining fees are separate. No server gets spending keys or prematurely disclosed preimages.

## Completed work

1. Pinned, checksummed, and ran upstream nodes; verified active Blake2b consensus and both signature algorithms against actual nodes.
2. Implemented deterministic wallet branches, HTLCs, signed transactions, validation, and encrypted durable state.
3. Implemented persistent Nostr relays, signed offer projection, and encrypted acknowledged mailboxes.
4. Implemented the asynchronous swap engine, cancellation/reservation, restart and reorg recovery, and delayed claim/refund rescue.
5. Built and exercised the native SwiftUI application over the daemon's private Unix socket API.
6. Passed the unit, race, vector, fuzz, and actual two-chain integration harness. Completed a fresh trade through the final native build and independently verified both confirmed claims against the nodes.
7. Documented the protocol, recovery, operation, fees, assumptions, limitations, and invariant coverage.

The local-regtest goal was verified on September 5, 2026. Both nodes, two relays, independent Alice/Bob daemons, the watchtower, and the native application were left running. The final GUI trade exchanged 1,000,000 BTC sats for 2,000,000 BLAKE sats with two confirmations on both claims and zero tower payment. See [the verification record](VERIFICATION.md) for exact transaction IDs and checks.

No finite test suite proves every possible invariant or adversarial schedule. Maintain an explicit invariant/coverage matrix, including failures and unresolved limitations, rather than claiming exhaustive proof.

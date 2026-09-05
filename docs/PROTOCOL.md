# Atomic swap protocol v1

## Scope and roles

An offer sells one exact amount on one chain for one exact amount on the other. There are no partial fills, market orders, AMMs, Lightning channels, trusted escrow, or account balances held by a matching service. This is an on-chain atomic swap, not a Lightning submarine swap.

**Maker/taker are market roles.** In this protocol the **taker always chooses the preimage and funds first**, regardless of which asset they sell. The taker funds the long-timeout HTLC; the maker funds the short-timeout HTLC. The protocol works in either BTC/BLAKE direction.

Offers are signed intent, not executable PSBTs. The original proposal of a pre-signed open order that an arbitrary future taker can complete is replaced by authenticated negotiation of concrete keys, amounts, outpoints, deadlines, and tower policy. A maker can post an offer and stop its daemon, but it must return to accept and fund a specific swap. The participants can take turns being online; they need not overlap. Once funded, timeouts constrain that asynchrony.

## Contracts and signatures

For a fresh 32-byte random secret `s`, `H = SHA256(s)`. Each chain uses this exact native P2WSH witness script:

```
OP_IF
  OP_SIZE 32 OP_EQUALVERIFY
  OP_SHA256 <H> OP_EQUALVERIFY
  <claim compressed public key> OP_CHECKSIG
OP_ELSE
  <refund height> OP_CHECKLOCKTIMEVERIFY OP_DROP
  <refund compressed public key> OP_CHECKSIG
OP_ENDIF
```

The claim witness is `[signature, s, 0x01, witnessScript]`. The refund witness is `[signature, empty, witnessScript]`. Claims need the correct secret and claimant signature; refunds need the funder's signature and an eligible locktime. Neither condition alone grants the tower a new signing capability.

All application-generated transactions are version 2. Inputs use sequence `0xfffffffd`, making `nLockTime` effective and signaling replacement. Funding consumes confirmed local P2WPKH coins, creates the HTLC at output 0, and returns non-dust change to the chain's local deposit key. No spending key crosses node RPC. Refunds are signed and persisted before funding is broadcast.

BTC signatures use BIP-143 and hash byte `0x01`. Blake2b signatures use hash byte `0x21` and the fork's `UnifiedSighash` tagged hash. The implementation supports only ALL and SegWit v0. It commits to every input outpoint, spent value/script, input sequence, output, transaction version, and locktime; the unified message zero-extends locktime to five bytes. None, Single, AnyoneCanPay, Taproot, and custom script modes are outside v1.

The two contracts share `H` but have different keys and assets:

| Contract | Funder/refunder | Claimant | Principal |
| --- | --- | --- | --- |
| Long, on the asset the maker buys | Taker | Maker | Offer's buy amount |
| Short, on the asset the maker sells | Maker | Taker | Offer's sell amount |

## Authenticated terms

The request contains a random swap ID, the original signed open-offer event, taker Nostr identity, `H`, and two taker per-swap compressed public keys. The maker verifies the request, its own current open offer, available funds, and configured tower quote. It reserves the entire offer exactly once, derives its keys, and sends immutable accepted terms.

Terms include the full request, both contracts before funding, both maker keys, both application chain domains, both refund heights, the long-chain reveal cutoff/tower takeover height, and selected tower identity/payout scripts. JSON structs are serialized deterministically by Go and SHA256 hashed to bind subsequent messages. There are no floating-point amounts or prices: all amounts and basis points are integers. An implementation in another language must reproduce this serialization or negotiate a future canonical encoding version.

The taker checks that acceptance preserves its exact request, the signed maker offer, keys, amounts, hash, domains, and locally configured tower quote. A changed contract requires a new negotiation, not reinterpretation of a signature already handed out.

## Regtest deadline policy

At acceptance, let `L0` and `S0` be current heights on the long and short chains. Do not compare their numerical heights to each other.

| Quantity | Local-chain tip threshold |
| --- | --- |
| Long refund `Lr` | `L0 + 96` |
| Short refund `Sr` | `S0 + 48` |
| Tower long-claim takeover | `L0 + 32` |
| Honest taker's last reveal window | Before `L0 + 24` |
| Tower refund takeover | Respective refund threshold + 6 |
| Funding and settlement confidence | 2 confirmations on each chain |

Gate checks are repeated before irreversible actions:

- Before long funding: at least 84 long-chain blocks and 40 short-chain blocks remain before refunds.
- Before short funding: at least 64 long-chain and 32 short-chain blocks remain, and at least eight long-chain blocks remain to the reveal cutoff.
- Before first revelation: both exact funding outputs are still unspent with two confirmations, at least 48 long-chain and 16 short-chain blocks remain, and the long-chain tip is strictly before the reveal cutoff.

These are local demonstration parameters, not calibrated mainnet security recommendations. Relative chain progress, censorship, congestion, and reorg risk still matter. Mining one regtest chain far ahead intentionally demonstrates failures of timing assumptions.

Height locktimes use Bitcoin's strict finality comparison. A transaction with `nLockTime=T` is eligible for the next block when the current tip reaches `T`; its first eligible block has height `T+1`. The GUI displays tip thresholds. A refund becoming eligible does not invalidate a claim: they can compete for the same UTXO.

## Happy-path state transitions

```mermaid
sequenceDiagram
  participant M as Maker daemon
  participant R as Nostr relays
  participant T as Taker daemon
  participant W as Watchtower
  participant L as Long chain
  participant S as Short chain
  M->>R: Signed open offer
  T->>R: Encrypted request with H and taker keys
  R->>M: Request delivered when maker returns
  M->>R: Reserve offer and encrypt accepted terms
  R->>T: Accepted terms
  T->>W: Encrypted long-refund rescue job, no secret
  W->>T: Durable receipt
  T->>L: Signed long funding; refund already persisted
  T->>R: Long funding notification
  R->>M: Funding details
  M->>L: Verify exact output and confirmations
  M->>W: Short refund and delayed long claim templates
  W->>M: Durable receipts
  M->>S: Short funding; refund already persisted
  M->>R: Short funding notification
  R->>T: Funding details
  T->>L: Verify long output and headroom
  T->>S: Verify short output, then claim and reveal s
  M->>S: Observe s in mempool or chain
  M->>L: Claim incoming funds without tower fee
```

A configured tower's durable receipt is required before its protected party funds. A receipt commits to the exact validated job digest; relay `OK` acknowledgments alone do not arm protection. With tower protection explicitly disabled, the daemon still prepares its own refunds but does not wait for a tower.

## Delayed tower claims

The maker signs alternative spends of its incoming long HTLC. Each has a fixed owner payout, a fixed percentage bounty to the selected tower, a bounded mining fee, and `nLockTime` equal to takeover. The claim signature is handed over **without the secret**. The tower fills that witness element only after observing the matching preimage on the other chain.

Changing the delay, inputs, sequences, owner destination, bounty, or other outputs invalidates the signature. Broadcasting bytes early cannot make them mineable early. The owner can create a fee-free claim and have it confirmed before takeover; that permanently invalidates every competing fallback. The tower cannot append its fee to an already-confirmed owner claim.

The taker's first-reveal transaction is never handed to the tower containing a still-private secret. A timelock controls confirmation, not information disclosure: giving away that transaction early would leak `s` and undermine the swap.

The tower's clock starts at a pre-agreed local-chain threshold. It does not restart when the other chain reveals a secret. The application reveal cutoff normally leaves a grace window; it is not enforced by the HTLC script against a malicious taker. “Tower needed” means the output remains available when the delayed transaction becomes eligible, not proof that its owner was offline.

Refund rescue jobs similarly spend the party's own HTLC after the refund threshold plus six blocks. The owner can refund first without a bounty. No hash preimage is needed for a refund job.

## Messaging and crash recovery

Public offers use addressable kind `38481`. Latest `created_at` wins within `(kind, author, d)`, with the lexicographically lower event ID breaking equal-time ties. Status changes publish `reserved`, `cancelled`, or `filled`. Local expiry is enforced even if a relay retains an old event. A relay deletion request is never treated as revoking an on-chain capability.

Private messages use a versioned application envelope with stable message ID, type, swap ID, and JSON body. Current types are `request`, `accepted`, `rejected`, `long-funded`, `short-funded`, `tower-job`, `tower-receipt`, and `ack`. They are unsigned inner rumors, authenticated by their signed encrypted seal, inside encrypted signed gift wraps.

The sender saves ciphertext and protocol state before sending. Processing records are keyed by authenticated sender and message ID, with a digest that rejects changed contents under the same ID. Duplicates repeat acknowledgments and cannot create another reservation or funding transaction. A recipient's authenticated application acknowledgment is separate from relay storage acknowledgment. Pending messages retry across both configured relays and survive process restarts. Current synchronization replays bounded retained history; it is not a scalable cursor protocol.

Funding, refund templates, jobs, receipts, and first-reveal intent are committed before the associated broadcast. Failure to persist stops execution. The scanner handles confirmed and mempool spends; a reorg reduces confirmations or removes settlement observations. The fact that a secret was disclosed remains permanent. A reserved or filled offer is not reopened automatically after a reorg.

## Cancellation and timeout outcomes

An unreserved offer can be cancelled by an authenticated local command; its signed cancellation is relayed and the maker refuses stale requests. Offers have no committed funding UTXO, so this cancellation does not need a chain transaction. Cancellation and reservation are serialized locally; whichever commits first wins. Public relays can temporarily disagree.

After acceptance/funding, “cancel” cannot revoke an HTLC or erase a signature another party possesses. The protocol completes a claim or waits for refunds. If the taker never reveals, both parties reclaim their respective outputs after timeout, with towers available after their extra grace. If a party misses a deadline after the secret has been released, an adverse claim/refund race can break the intended economic exchange. There is no arbitration service that can reverse chain settlement.

## Primary references

- [BIP-65: CHECKLOCKTIMEVERIFY](https://github.com/bitcoin/bips/blob/master/bip-0065.mediawiki)
- [BIP-143: SegWit signature hashing](https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki)
- [Pinned Bitcoin Blake2b unified signature specification](https://github.com/bitcoinknots/bitcoin/blob/v29.4.1.knots20260508/doc/unified-sighash.md)
- [NIP-01: event and relay protocol](https://github.com/nostr-protocol/nips/blob/master/01.md)
- [NIP-44: encrypted payloads](https://github.com/nostr-protocol/nips/blob/master/44.md)
- [NIP-59: gift wrapping](https://github.com/nostr-protocol/nips/blob/master/59.md)

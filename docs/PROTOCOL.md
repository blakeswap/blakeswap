# Atomic swap protocol

## Scope and roles

An offer is a signed parent order with a fixed sell/buy ratio. It explicitly permits either one whole fill or partial fills within signed minimum and maximum quantities. Each accepted fill becomes its own on-chain atomic swap. There are no market orders, AMMs, Lightning channels, trusted escrow, or account balances held by a matching service.

The unreleased application uses protocol format 2, public API v2 and vault state format 3. Missing and legacy formats are rejected; there is no version negotiation, mixed-client execution or automatic development-state migration. Existing files and credentials are preserved on refusal. Use a separate development profile when a fresh state is required. Wallet derivation, chain replay domains, the private app/helper protocol and portable encryption envelopes have separate identities and do not change with this public cutover. See the [partial-fill state design](PARTIAL_FILLS.md) and [API contract](PARTIAL_FILL_API.md).

**Maker/taker are market roles.** In this protocol the **taker always chooses the preimage and funds first**, regardless of which asset they sell. The taker funds the long-timeout HTLC; the maker funds the short-timeout HTLC. The protocol works in either BTC/BLAKE direction.

Offers are signed intent, not executable PSBTs. The original proposal of a pre-signed open order that an arbitrary future taker can complete is replaced by authenticated negotiation of concrete keys, amounts, outpoints, and deadlines. A maker can post an offer and stop its daemon, but it must return to accept and fund a specific swap. The participants can take turns being online; they need not overlap. Once funded, timeouts constrain that asynchrony.

## Contracts and signatures

For a fresh 32-byte random secret `s`, `H = SHA256(s)`. Each chain uses this exact native P2WSH witness script:

```
OP_IF
  OP_SIZE 32 OP_EQUALVERIFY
  OP_SHA256 <H> OP_EQUALVERIFY
  <claim compressed public key> OP_CHECKSIG
OP_ELSE
  <refund locktime> OP_CHECKLOCKTIMEVERIFY OP_DROP
  <refund compressed public key> OP_CHECKSIG
OP_ENDIF
```

The claim witness is `[signature, s, 0x01, witnessScript]`. The refund witness is `[signature, empty, witnessScript]`. Claims need the correct secret and claimant signature; refunds need the funder's signature and an eligible locktime. Neither condition alone grants the tower a new signing capability.

All application-generated transactions are version 2. Inputs use sequence `0xfffffffd`, making `nLockTime` effective and signaling replacement. Funding consumes confirmed local P2WPKH coins, creates the HTLC at output 0, and returns non-dust change to the chain's local deposit key. No spending key crosses node RPC. Refunds are signed and persisted before funding is broadcast.

BTC signatures use BIP-143 and hash byte `0x01`. Blake2b signatures use hash byte `0x21` and the fork's `UnifiedSighash` tagged hash. The implementation supports only ALL and SegWit v0. It commits to every input outpoint, spent value/script, input sequence, output, transaction version, and locktime; the unified message zero-extends locktime to five bytes. None, Single, AnyoneCanPay, Taproot, and custom script modes are outside this protocol.

The two contracts within a child share `H` but have different keys and assets. Separate children use distinct hashes, secrets, keys, funding outpoints and tower jobs. With parent sell amount `A`, buy amount `B` and requested sell quantity `q`:

| Contract | Funder/refunder | Claimant | Principal |
| --- | --- | --- | --- |
| Long, on the asset the maker buys | Taker | Maker | `ceil(q * B / A)` |
| Short, on the asset the maker sells | Maker | Taker | `q` |

The product and division use exact wide integer arithmetic. Each fill preserves the maker's signed rate; independently rounded buy amounts may sum to more than the parent price-reference `B`. Every fill must satisfy the signed bounds, per-leg principal limits, actual fee/tower economics and a remainder that can still be partitioned into legal fills. Whole mode requires `q=A`.

## Authenticated terms

The request contains format 2, a random child swap ID, the complete exact signed open-offer event, its revision, requested quantity `q`, taker Nostr identity, `H`, and two distinct taker per-swap compressed public keys. The maker verifies the request, its current signed availability revision, real funding inputs and privately stored protection and monetary limits. It durably reserves that slice, assigns disjoint whole inputs, derives its keys, and saves immutable accepted terms and their outgoing message before acceptance can leave the wallet. Racing takes cannot overdraw the parent; a stale revision requires a new review.

Terms include the full request, both contracts before funding, both maker keys, both application chain domains, both refund locktimes, the long-chain reveal cutoff/tower takeover locktime, without either party’s tower identity, fee, payout scripts, quote, or protection flag. JSON structs are serialized deterministically by Go and SHA256 hashed to bind subsequent messages. There are no floating-point amounts or prices: all amounts and basis points are integers. An implementation in another language must reproduce the current serialization exactly; this format provides no encoding negotiation.

The taker checks that acceptance preserves its exact request, the signed maker offer, keys, amounts, hash, domains, and protocol version. Each wallet independently pins its own provider quote in encrypted local state; that selection is never sent to the counterparty. A changed contract requires a new negotiation, not reinterpretation of a signature already handed out.

The parent maintains `Total = Available + Reserved + Committed + Filled + Released`. These are mutually exclusive current quantities. Persisting own funding bytes moves a child to Committed before any possible broadcast. Positive settlement and child-specific reorg evidence update that child's allocation without replenishing permanently consumed fee or bounty authorization. A wallet with one large coin may have advertised inventory but insufficient independent inputs for simultaneous children. Confirmed funding change can support another child, creating a real ancestry dependency that must be reconciled after a reorg. The full transition table and recovery requirements are in [Partial fills](PARTIAL_FILLS.md#quantity-ownership).

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

## Public-network deadline policy

Mainnet and Testnet4 use time-based CLTV, not cross-chain block-count comparisons.
Read BIP-113 median time past (MTP) from each chain. At acceptance let `T0` be the
larger MTP, after checking the clocks differ by at most two hours. Terms separately
record each chain's current height for observation scans; timestamps must never
be used as scan heights.

| Quantity | Unix locktime / policy |
| --- | --- |
| Long refund | `T0 + 4 days` |
| Short refund | `T0 + 2 days` |
| Tower long-claim takeover | `T0 + 24 hours` |
| Last honest first-reveal window | Strictly before `T0 + 12 hours` on the long chain |
| Tower refund takeover | Own refund locktime + 6 hours |
| Funding / settlement confidence | 6 confirmations on each chain |

A timestamp transaction becomes eligible only when the preceding block's MTP is
**strictly greater** than its locktime. Wall-clock passage alone does not unlock
funds. Both chain clocks must be within two hours of each other and within the
range local wall clock minus six hours to plus two hours for new funding/revelation.
The local wall clock is therefore also an availability dependency. These checks
do not block already-authorized refunds or rescue attempts.

Before long funding, at least 94 long-chain hours and 46 short-chain hours remain.
Before short funding, at least 72 and 24 hours remain, and two hours remain before
the reveal cutoff. Before first revelation, at least 48 and 12 hours remain and
the long-chain MTP is strictly before the cutoff. A proposed schedule starting
more than two hours beyond the latest observed MTP is rejected. These gates run
again immediately before broadcasting new funding or committing first revelation.
Exact UTXOs, six confirmations, BTC replay ancestry, keys, amounts, network domains,
and optional durable tower receipts are checked independently.

This replaces the regtest assumption of comparable block rates with explicit
clock and bounded-response assumptions. It does not guarantee settlement under
arbitrary hash-rate loss, clock manipulation, reorgs, congestion, or censorship.
The four/two-day schedule and six confirmations are implemented policy, not an
independently calibrated economic security guarantee.

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
  T->>R: Exact parent revision, q, H and taker keys
  R->>M: Request delivered when maker returns
  M->>R: Persist slice/input reservation and accepted terms
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

A configured tower's durable receipt is required before its protected party funds. A receipt commits to the exact validated job digest; relay `OK` acknowledgments alone do not arm protection. With local tower protection disabled, that wallet still prepares its own refunds but does not wait for a tower. A maker’s choice protects only the maker. A taker chooses its own optional refund protection through the local take command. Neither choice changes the public offer or shared terms.

## Delayed tower claims

The maker signs alternative spends of its incoming long HTLC. Each has a fixed owner payout, a fixed percentage bounty to the selected tower, a bounded mining fee, and `nLockTime` equal to takeover. The claim signature is handed over **without the secret**. The tower fills that witness element only after observing the matching preimage on the other chain.

Changing the delay, inputs, sequences, owner destination, bounty, or other outputs invalidates the signature. Broadcasting bytes early cannot make them mineable early. The owner can create a fee-free claim and have it confirmed before takeover; that permanently invalidates every competing fallback. The tower cannot append its fee to an already-confirmed owner claim.

The taker's first-reveal transaction is never handed to the tower containing a still-private secret. A timelock controls confirmation, not information disclosure: giving away that transaction early would leak `s` and undermine the swap.

The tower's clock starts at a pre-agreed local-chain threshold. It does not restart when the other chain reveals a secret. The application reveal cutoff normally leaves a grace window; it is not enforced by the HTLC script against a malicious taker. “Tower needed” means the output remains available when the delayed transaction becomes eligible, not proof that its owner was offline.

Refund rescue jobs similarly spend the party's own HTLC after the refund threshold plus six blocks on regtest or six hours on public networks. The owner can refund first without a bounty. No hash preimage is needed for a refund job.

## Messaging and crash recovery

Public offers use addressable kind `38481` in the `blakeswap-<network>-v2`
namespace. Their signed schema contains format, network, ID, maker, amounts,
asset, expiry, status, explicit fill mode/bounds, revision and available quantity.
It contains no single-child reservation or private fee/bounty/protection fields.
The maker’s authenticated local API overlays its own saved choice; other wallets
receive no provider or protection information.
Only encrypted jobs and receipts disclose an order’s protection to its provider.
Provider directory announcements describe a service, not which orders use it.

Old or missing versions and unsupported public fields are rejected. Existing
network state, including protocol-bearing cold records, must pass authenticated
format checks before use; an incompatible profile is refused before credential
journal or Keychain item changes. It is not silently rewritten, reset or used to
resume a publisher. Prior disclosures stored by third parties cannot be erased.
Public blockchain rescue transactions can still reveal a provider payout when a
rescue is executed; private selection does not hide that transaction.

Latest `created_at` wins within `(kind, author, d)`, with the lexicographically lower event ID breaking equal-time ties. A payload revision does not override this ordering. A parent signs at most one availability update per wall-clock second; pending updates coalesce and hold new admission until the corresponding signed revision is available. Clock rollback holds publication rather than inventing future timestamps. Accepted children continue settling. Public `open`, `reserved`, `cancelled` and `filled` views summarize the parent; a single settled child cannot stand for all children. Local expiry and cancellation take effect even if the relay view is stale. A relay deletion request never revokes an on-chain capability.

Private messages use a versioned application envelope with stable message ID, type, swap ID, and JSON body. Current types are `request`, `accepted`, `rejected`, `long-funded`, `short-funded`, `tower-job`, `tower-receipt`, `tower-query`, `tower-quote`, and `ack`. They are unsigned inner rumors, authenticated by their signed encrypted seal, inside encrypted signed gift wraps.

Mailbox reads support [NIP-42 relay authentication](https://github.com/nostr-protocol/nips/blob/master/42.md). If a configured relay rejects a subscription with `auth-required:`, the daemon signs a kind `22242` authentication event with its network-specific Nostr identity. The event binds the configured relay URL, the connection's challenge, and the current time. Only a matching successful authentication acknowledgment permits one subscription retry. Unsolicited challenges alone do not trigger authentication; rejected or missing acknowledgments do not count as successful reads. This authentication proves control of the recipient identity without sharing its secret key. Gift-wrap publishing remains unauthenticated to avoid linking the sender's identity to its outgoing encrypted envelopes; relays requiring authenticated writes are currently unsupported.

The sender saves ciphertext and protocol state before sending. Processing records are keyed by authenticated sender and message ID, with a digest that rejects changed contents under the same ID. Retained child identity also binds the exact original request across active and archived placement. An exact accepted retry repeats only its immutable acceptance; a retired child stays closed. Changed quantity, revision, peer or keys under an existing identity cannot create another reservation or funding transaction. A recipient's authenticated application acknowledgment is separate from relay storage acknowledgment. Pending messages retry across both configured relays and survive process restarts. See [capacity and archival](OPERATIONS.md#encrypted-history-and-capacity) for bounded synchronization and retained-history limits.

Funding, refund templates, jobs, receipts, and first-reveal intent are committed before the associated broadcast. Failure to persist stops execution. The scanner handles confirmed and mempool spends; a reorg reduces confirmations or removes settlement observations. The fact that a secret was disclosed remains permanent. A contradicted child settlement retains its funding obligation and cannot automatically restore available inventory. Unrelated siblings keep their own evidence; funding descendants need their dependent proofs reconciled.

## Cancellation and timeout outcomes

An authenticated parent cancellation withdraws only its still-Available quantity and unassigned input/fee reserve. Accepted Reserved or Committed children, exact receipts, permanent charges and signed obligations remain. Cancellation and admission serialize locally; public relays can temporarily disagree. A freshly reviewed remainder replacement creates a new parent without deleting the old accepted children or reusing their assigned inputs.

A never-funded maker child can retire only after positive evidence that its funding gate has closed and a durable irreversible refusal of future own funding. It retains its original quantity and authenticated peer history, but has zero current allocation if returned to an open parent's Available bin. For a closed parent, its allocation moves to Released. Late peer funding and refunds remain observable without reviving maker funding or a second allocation. Missing peers, unavailable indexers and absent transactions do not prove that funding was never published; imported uncertainty cannot assert this safe-return condition.

After acceptance/funding, “cancel” cannot revoke an HTLC or erase a signature another party possesses. The protocol completes a claim or waits for refunds. If the taker never reveals, both parties reclaim their respective outputs after timeout, with towers available after their extra grace. If a party misses a deadline after the secret has been released, an adverse claim/refund race can break the intended economic exchange. There is no arbitration service that can reverse chain settlement.

## Primary references

- [BIP-65: CHECKLOCKTIMEVERIFY](https://github.com/bitcoin/bips/blob/master/bip-0065.mediawiki)
- [BIP-113: median time past locktime semantics](https://github.com/bitcoin/bips/blob/master/bip-0113.mediawiki)
- [BIP-143: SegWit signature hashing](https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki)
- [Pinned Bitcoin Blake2b unified signature specification](https://github.com/bitcoinknots/bitcoin/blob/v29.4.1.knots20260508/doc/unified-sighash.md)
- [NIP-01: event and relay protocol](https://github.com/nostr-protocol/nips/blob/master/01.md)
- [NIP-44: encrypted payloads](https://github.com/nostr-protocol/nips/blob/master/44.md)
- [NIP-59: gift wrapping](https://github.com/nostr-protocol/nips/blob/master/59.md)

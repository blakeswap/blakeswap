# Fees and transaction recovery

Fees are native satoshis: BTC pays BTC fees and BLAKE pays BLAKE fees. No exchange
rate or estimate from the other chain is used. Send, Create offer, and Take offer
show the recipient/contract amount, total mining fee, change, and a conservative
virtual-size bound. The selected fee stays exact; dust change is rejected rather
than silently donated to miners.

## Estimates and manual selection

`QuoteFee` (`POST /v1/fees/quote`, CLI `fee.quote`) takes `kind` (`send` or
`funding`), `chain`, `amount`, optional `destination`, optional selected `inputs`,
`fee`, `target`, and `expected_network`. Sends require explicit inputs. Funding
selects unlocked confirmed candidates; the quote does not reserve or sign them.
A positive `fee` requests a manual total. Zero requests an estimate. Default
confirmation target is six blocks.

RPC `estimatesmartfee` and Electrum `blockchain.estimatefee` return native coin
units per 1,000 virtual bytes on the supported SegWit chains. The RPC help on both
pinned nodes explicitly specifies BIP-141 virtual size; Blake's inherited RPC help
uses the label BTC, but its amounts are BLAKE. Electrum's
[protocol specification](https://electrumx.readthedocs.io/en/latest/protocol-methods.html#blockchain-estimatefee)
defines native coin units per kilobyte and `-1` for insufficient data. The daemon
parses bounded decimal/scientific notation directly, multiplies by 100,000,000,
and rounds upward to integer `rate_sat_kvb`. For example, `0.00006539` becomes
6,539 native sat/kvB (6.539 sat/vB), not 6,539 sat/vB.

The reply contains chain, source method, requested target, effective RPC target,
observation timestamp, and `available`, `unavailable`, or `stale` estimate state.
Errors never synthesize a rate. Manual selection remains available when estimates
fail. Estimates expire after 120 seconds, and future timestamps are rejected.
When using an estimate, pass its `rate_sat_kvb` and `fee_timestamp` with the exact
reviewed total. Send signing and new offer/request authorization reject stale or
undersized reviews. Wallet inputs are P2WPKH; fee bounds include every selected
input, a maximum-length signature, CompactSize prefixes, witness overhead, and the
actual destination/change script lengths. Contract funding uses a 34-byte P2WSH
output. Advisory quotes and replay preflights do network IO outside the desktop
manager/engine locks; the signing path independently rechecks funds and ancestry.

## Authorizations and caps

`Status.fee_limits` reports explicit per-chain native-satoshi limits:

| Transaction | BTC cap | BLAKE cap | Authorization |
| --- | ---: | ---: | --- |
| Wallet send | 1,000,000 | 1,000,000 | Exact initial total and user-selected `max_fee` |
| Swap funding | 100,000 | 100,000 | Exact `funding_fee` persisted before offer/request reservation |
| Owner claim/refund | 20,000 | 20,000 | Explicit `owner_fee_cap=20000` permits the bounded ladder |
| Tower rescue | 20,000 | 20,000 | Exact signed v1 templates; unchanged fixed bounty and recipient |

Omitted send `max_fee` authorizes only the initial fee. Omitted `funding_fee`
retains the legacy 2,000-sat total. Omitted owner cap retains base-only automatic
settlement. The native swap forms explicitly disclose and submit the 20,000-sat
owner cap. Owner and tower ladders remain 2,000, 6,000, and 20,000 sats; larger caps
require a future protocol/policy extension. Principal and tower bounty never
change; increasing a settlement fee reduces only the owner's authorized net
payout. Funding fees are separate from contract principal and remain private local
policy, outside public offers and peer terms.

A selected funding fee, rate floor, and owner cap survive restart and delayed
acceptance. A later market estimate never rewrites an existing funding fee. Input
count changes that exceed its reviewed rate allowance block funding with the
reservation intact. Existing v1 terms, raw funding, refund bundles, and tower jobs
keep their original meanings. Old claims do not acquire a new replacement budget
on upgrade. Previously signed refund variants remain available for explicit
selection. No automatic change to the tower's signed limits or bounty occurs.

## Recovery and states

`BumpTransaction` (`POST /v1/transactions/bump`, CLI `transaction.bump`) takes an
activity `id`, `kind`, higher total `fee`, `expected_txid`, and `expected_network`.
Use the current status transaction ID; a stale ID cannot authorize a new variant.

Wallet sends preserve the same inputs, recipient script, and recipient amount.
Only change pays the increase. A send without enough non-dust change, without
saved signing inputs/authorization, above its original cap, or beyond 16 signed
variants is refused. BTC replacements repeat replay-ancestry verification. The
increment must cover at least one native sat/vB; configured nodes can require
more. Repeating a request for an existing fee returns its existing variant.
Every signed variant is persisted before broadcast, retained, and observed.

Outgoing states distinguish `saved` (including ambiguous submission), `broadcast`,
`stuck` (unconfirmed after two minutes), `confirmed`, and `unknown` (lookup failure).
Broadcast acceptance never means confirmation. Status lists all variant IDs,
fees, and confirmations. Observation continues beyond six confirmations, so deep
reorgs can restore earlier variants. A confirming older variant wins the displayed
outcome; after a reorg the highest authorized variant can resume. Absence from one
mempool never releases original input reservations or deletes signed evidence.
The two-minute label is a recovery prompt, not a claim about expected block time.

Newly authorized owner settlements retry no faster than every 30 seconds and move
up one tier after three attempts. A current chain estimate can select a higher
already-authorized tier immediately. Manual claim/refund buttons select signed
variants within the same cap. The tower similarly uses its pre-signed ladder,
five-second retries and three attempts per tier, with per-chain estimates bounded
by its signed cap. Attempted tower variant IDs are durable even if broadcast
responses are lost. Claim secrets remain monotonic through eviction and reorg.

**Funding acceleration is explicitly unsupported.** Replacing funding changes its
transaction ID and would invalidate refunds, peer outpoints, and tower jobs.
There is no generic funding RBF or CPFP button. Retain the exact funding
transaction and recovery bundle, keep the daemon running, and restore node/indexer
connectivity for identical retries. No absence/timeout permits forgetting a
potentially confirming funded obligation. Supporting funding CPFP or re-acknowledged
replacement protections requires a separately validated strategy.

## Validation and operating limits

Fee regression tests cover bounded/exact decimals, source units, effective targets,
freshness, input/script sizes, dust, caps, duplicates, ambiguous broadcasts,
durable-before-broadcast ordering, legacy owner consent, restart, previous-variant
confirmation, and deep reorg observation. Real BTC and activated Blake2b tests use
isolated regtest wallets: a one-sat send is rejected below each node's relay policy,
then its authorized 6,000-sat replacement and 20,000-sat RBF variant are accepted,
mined, invalidated, and reconsidered. Separate real claim/refund/tower ladders
replace mempool spends while preserving the original funding outpoint and signed
recovery, principal, payout script, and bounty. RPC and Electrum paths are tested;
without `BLAKESWAP_REGTEST`, those tests skip.

Estimates and fee caps cannot guarantee confirmation during arbitrary congestion,
operator outages, censorship, or pinning. Send cancellation, arbitrary extra-input
RBF, CPFP, funding replacement, and higher v1 tower caps are unsupported. Keep
state backups: a mnemonic alone does not preserve signed obligations and lineage.

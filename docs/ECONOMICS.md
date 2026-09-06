# Watchtower economic model

## What is paid

There is no DAO, native token, treasury output, matching fee, or upfront service payment. Each wallet can select no tower or privately pin its own provider's percentage quote. The maker's choice protects the maker; the taker independently chooses optional refund protection. These choices do not appear in the public offer or shared swap terms. The demo provider quotes 50 basis points, or 0.50%; the protocol permits 1–1000 basis points for a protected order.

For the specific local-chain output being rescued:

```
bounty_sats = ceil(principal_sats × basis_points / 10,000)
owner_payout = principal_sats − bounty_sats − mining_fee_sats
```

Amounts stay in their native chain's units. A BTC output pays BTC; a Blake2b output pays Blake2b. No price oracle converts the fee between chains. The maker's incoming claim and either party's refund may therefore pay different assets. A basis point is 0.01 percentage points.

The tower earns the bounty only if a transaction containing its pre-agreed payout is confirmed. The owner's ordinary claim or refund has no bounty output. Network mining fees still apply in either path. The tower needs no inventory to exchange against users and does not hold a trading principal.

## Example

Alice offers 1,000,000 BTC sats for 2,000,000 Blake2b sats. Bob funds the long Blake2b HTLC and Alice funds the short BTC HTLC. Bob claims BTC and reveals the secret.

| Alice's incoming Blake2b outcome | Alice receives | Tower receives | Miner receives |
| --- | ---: | ---: | ---: |
| Alice claims herself, base fee | 1,998,000 sats | 0 | 2,000 sats |
| Delayed tower claim, base fee | 1,988,000 sats | 10,000 sats | 2,000 sats |
| Delayed tower claim, second fee variant | 1,984,000 sats | 10,000 sats | 6,000 sats |
| Delayed tower claim, highest fee variant | 1,970,000 sats | 10,000 sats | 20,000 sats |

Funding fees are paid separately by each funder. If no revelation occurs and the tower eventually refunds Alice's 1,000,000 BTC-sat output, that refund's bounty is 5,000 BTC sats. Bob's long-output refund would carry a 10,000 Blake2b-sat bounty. These are separate payments in different assets, not a common-currency total.

The transaction's signer explicitly authorizes the exact bounty and bounded fee variants. A tower cannot turn the percentage into an arbitrary amount or redirect the owner's balance. A tower may also be paid if someone else broadcasts its authorized transaction; chains cannot identify which network participant first relayed a transaction.

## “Only if needed” has a precise boundary

The consensus-enforceable condition is: **the delayed signed fallback confirms while the output is still spendable**. The chain cannot prove that the owner was offline, failed to notice the secret, or tried to broadcast first.

An owner claim confirmed before takeover guarantees no fee. An owner transaction merely sitting in the mempool does not: after takeover, the fallback can compete, and mining/relay policy decides which spend confirms. The owner remains able to sign a fee-free claim after takeover, but its confirmation is no longer assured. This is intentional competition over one UTXO, not a reservation of a block slot.

Tower receipts record acceptance of a specific job. They are signed promises, not cryptographic proof of future uptime or permanent disk durability. No bond, insurance, slashing, or reimbursement mechanism is included.

## Provider viability

Let `p` be the probability a job needs a fallback, `q` the probability the provider wins confirmation after becoming eligible, `V` the rescued principal's value to the provider, `r` the percentage rate, and `C` its monitoring/storage/network cost. A simple expected gross contribution is:

```
expected contribution ≈ p × q × V × r − C
```

This is a model, not a claim of profitability. Most protected trades can finish with no tower payout, while monitoring costs are incurred on every accepted job. A provider must choose its own rate, minimum amount, accepted assets, job capacity, relay redundancy, fee policy, and reliability target. Percentage-only fees make tiny trades and frequently abandoned jobs unattractive. Holding BLAKE-denominated bounties also exposes providers to that asset's price and liquidity risk.

The application enforces a conservative 600-sat minimum bounty and non-dust owner payout after the highest pre-authorized mining fee. At 50 basis points the bounty floor alone implies a 120,000-sat minimum principal. The ordinary order minimum is 100,000 sats, so protection can impose a stricter minimum. No minimum fixed fee is silently charged in addition to the percentage.

The local tower caps stored jobs at 1,000 and the relay caps records. These are development guardrails, not a complete public-service anti-spam or pricing strategy. An attacker can ask a provider to store valid but never-funded jobs. Upfront payment was deliberately excluded; production admission and incentive design remains an open problem.

Wallet sends have an estimated or manually selected total mining fee and no tower
bounty. A send that consolidates selected coins is still an ordinary paid
transaction. There is no automatic consolidation service.

## Fee escalation

The signed v1 rescue ladder remains 2,000, 6,000, and 20,000 native sats. Each
variant preserves principal, recipient, and tower bounty; only the owner's net
payout and transaction signature/ID change. Per-chain estimates can select a
higher authorized tier, and retries advance within the cap. New owner policies
require explicit consent before funding; old claims retain their original signed
fee. New funding totals are reviewed and persisted separately, with the original
2,000-sat default retained for legacy obligations.

Wallet send fee increases consume only change and cannot exceed the original
user-selected maximum. Funding replacements are explicitly refused because
refunds and tower jobs commit its transaction ID. See [Fees and recovery](FEES.md)
for units, limits, retry intervals, API parameters, replacement lineage, and
supported recovery paths. No estimate or bounded fee allowance guarantees
confirmation during arbitrary fee spikes or transaction pinning.

## Pre-trade review

The native maker and taker sheets obtain economics from the daemon. For the example
above and a selected 6,500-sat funding fee, Alice pays 1,006,500 BTC sats and Bob
pays 2,006,500 BLAKE sats. With a newly authorized 20,000-sat owner cap, Alice's
ordinary incoming receipt is bounded at 1,980,000–1,998,000 BLAKE sats; Bob's is
980,000–998,000 BTC sats. The 50-bps delayed maker rescue instead yields
1,970,000–1,988,000 BLAKE sats. The taker's selected tower provides refunds only:
the taker must make the first secret-revealing claim itself.

Refund ranges subtract the refund mining fee and any applicable rescue bounty
from that wallet's outgoing principal; the separate funding fee is already spent.
The quoted exchange rate is the exact ratio of received to paid principals,
accompanied by an approximate decimal, not a rate net of costs in two assets.
Future settlement fees are bounded by the selected policy rather than presented
as a guaranteed prediction. Existing signed obligations retain their original fee
rules. Before negotiation, lockup/reveal/takeover windows are labeled expected
policy; accepted terms later provide exact deadlines. Confirmation and relay
latency can change elapsed time, and no completion time is guaranteed.

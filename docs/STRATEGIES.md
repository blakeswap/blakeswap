# Inventory-aware market making

Open **Market → Inventory-aware market making** to review one opt-in two-asset strategy for
the selected wallet and network. It creates whole maker offers through the same
automatic-offer executor, durable confirmations and atomic replacement path as
[automatic offers](AUTOMATION.md). It runs only while the app-owned wallet daemon
is running. It does not take other makers' offers, borrow, lend or pool funds.

## Review and inventory

Review separate BTC and BLAKE target balances, minimum unlocked reserves,
maximum outstanding principal exposure, minimum/maximum offer sizes, lifetime
gross sell-volume limits, funding fees and hard funding-fee caps. Both directions
share a maximum number of outstanding quotes/swaps and separate lifetime BTC and
BLAKE fee/rescue allowances. Manual offers, sends and swaps share the same coin
reservations; all wallet swaps and quotes count toward exposure and concurrency.
An unresolved imported charge also consumes its principal and one slot, even if
its original live offer is quarantined. It is never counted twice when its swap
or quote is present.

Only positively observed confirmed wallet coins support new offers. Pending
change, incoming unconfirmed payouts and locked HTLC principal do not become
spendable inventory. Coin selection locks whole inputs: enough total balance is
not sufficient if the chosen inputs would leave less than the minimum unlocked
reserve. Split or add confirmed coins when the preview explains this condition.
The other asset must also retain its unlocked reserve.

Size is deterministic. For each sell asset, start with maximum offer size times
available confirmed balance divided by its target, round down, and clamp to the
reviewed whole-offer range. Remaining directional exposure and gross spending
allowance may reduce it further. If the minimum whole offer does not fit, that
direction pauses. The preview shows exact sell/buy satoshis, the BLAKE-per-BTC
ratio, source offer, reference event IDs and the reason for any refusal.

## Prices and costs

Choose a fixed exact positive BLAKE-per-BTC ratio or explicitly selected external
orderbook maker identities, with the same freshness, signed provenance, minimum
maker count and disagreement limits as automatic offers. There is no assumed
fiat valuation or guaranteed return. Signed references prove who published them;
chosen identities can collude, so hard rate limits remain necessary.

The reviewed base spread and its minimum/maximum are **per-side half-spreads**,
in basis points (1 bps = 0.01%). For each side, subtract the optional skew times
its relative inventory surplus from the base half-spread, then clamp to the
reviewed bounds. Relative surplus is the difference between each asset's
confirmed balance/target ratio, clamped to −1…1; this compares target attainment,
not a fiat value. BTC-selling quotes use reference × (1 + half-spread/10,000),
and BLAKE-selling quotes use reference × (1 − half-spread/10,000). Buy amounts
round conservatively to whole satoshis, and the resulting exact ratio must still
satisfy both hard rate limits. Targets affect sizing; skew is optional.

Before a new offer is saved, both children recheck fresh selected funding fees,
provider terms, source authority, shared funds and shared allowances under the
same engine lock. The preview uses the maximum funding fee conservatively; an
executable quote may fit at a lower current selected fee even when that worst
case preview does not. A funding fee of zero requires a fresh estimate and still
honors the hard cap. No signed authority is created from the provisional size
plan. Conditional rescue bounties have asset-specific worst-case allowances and
are not paid upfront.

Gross principal and per-chain fee/rescue charges are authorization consumption,
not realized trading volume or actual fees. Signed maker funding permanently
commits the charge; completion, refund, reorg, restart and archive placement do
not replenish it. Only a conclusively unfunded local cancellation or expiry can
release a reservation. Edits cannot lower caps below recorded reservations and
commitments. There is one durable strategy per wallet; edit its existing ID to
retain its accounting, rather than creating a new strategy with reset budgets.

## Pause, stop and recovery

Pause and Stop both first persistently revoke future child actions and pending
grants, then cancel eligible unreserved quotes. A racing request is checked
against the same strategy authority before acceptance. Accepted contracts retain
their exact terms and continue settlement/refund/rescue independently of the
strategy's enabled state. An enabled parent still appears in shutdown protection
while its children are temporarily paused.

Stale/incomplete chain or relay observations, sparse/conflicting references,
funding-fee caps, reserve/exposure limits and exhausted allowances prevent new
quotes and withdraw eligible old quotes. Configure consecutive failure and
replacement failure limits (1–20), plus a failure-rate threshold over the last
20 attempted actions (applied after five attempts). Crossing a threshold disables
both children and requires a new complete review to resume. Ordinary unchanged
quote checks are not failures. The daemon schedules from the current time,
persists pending request/successor IDs and performs at most one automatic action
per tick; offline time never creates a catch-up burst.

Portable and legacy imports hold and disable strategies before installation.
Known accepted/funded obligations still recover using the original signed terms.
Current chain recovery alone cannot resume trading: review limits for the new
profile and explicitly acknowledge that an older backup may omit later spending.
Imported reserved charges stay uncertain even after resumption. An exposure
settlement proof is held on import or contradictory observations and becomes
usable only after fresh positive settlement evidence; it never releases the
monetary charge. Disabling or saving disabled does not clear the import hold.

## Reports and API

Current inventory, outstanding exposure/quotes/swaps, charge totals, previews and
breaker decisions are available without scanning activity history. Choose
**Read confirmed activity report** for a separate cancellable five-second query.
It attributes T08 activity to exact owned maker swap and transaction identities,
keeps BTC and BLAKE separate, counts positively settled claimed volume, and shows
confirmed wallet-paid fees and conditional bounties separately. Refunded principal
is not completed trade volume. Unavailable, stale or reorged evidence is excluded;
this is observed accounting, not an assertion that missing costs were zero.
Partial, cancelled or changed-wallet/source results are discarded. Reporting
never changes spendable balances or replenishes authorization.

The typed API exposes `ListStrategies` (`strategy.list`, POST
`/v1/strategies/query`), `ReviewStrategy` (`strategy.review`, POST
`/v1/strategies/review`), `SaveStrategy` (`strategy.save`, PUT `/v1/strategies`),
`StopStrategy` (`strategy.stop`, POST `/v1/strategies/stop`) and `ReportStrategy`
(`strategy.report`, POST `/v1/strategies/report`). List requires expected wallet
and network. Review/save use the complete `StrategyEdit`: config, desired enabled
state, expected revision, restored-budget acknowledgement, and the returned exact
review digest for save. Stop/report bind the ID, wallet, network and revision.
On an uncertain save response, refresh the same ID; do not create a retry ID.
The two derived `AutomationConfig.strategy_id` children can be inspected through
the automation API, but their authority can only be edited through the strategy.

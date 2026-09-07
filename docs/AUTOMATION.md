# Automatic offers

Automatic offers are opt-in policies scoped to a wallet identity and network.
Open **Market → Automatic offers**, create or edit a policy, read its complete
authorization, then save it enabled or disabled. Each policy maintains whole,
fixed-size offers. It uses the existing noncustodial settlement protocol and runs
only inside the wallet's running daemon. Quitting, sleeping or disconnecting the
machine can stop checks; this creates no system service or external scheduler.

## Authorization

The review binds the policy ID, revision, wallet, network and all of these fields:

- Sell asset and exact per-offer sell satoshis. Direction cannot change on an
  existing policy; create a separate authorization for the other direction.
- Lifetime gross sell-volume limit, in sell-asset satoshis. Refunds do not
  replenish consumed authorization.
- Fixed, minimum and maximum **BLAKE per BTC** rates as positive integer ratios.
  Rates compare exactly, and the final rounded whole-satoshi buy amount must
  still lie within both hard bounds. Display decimals never authorize a price.
- Offer lifetime, from the check cadence up to seven days; minimum cadence of
  60–86,400 seconds; maximum 1–8 outstanding policy offers. Accepted swaps count
  against this limit until terminal, even if their original quote has expired.
- A fixed total funding fee or zero to require a fresh network estimate, plus
  a hard maximum funding fee in sell-chain satoshis.
- Separate lifetime BTC and BLAKE fee/rescue budgets, each in its own chain's
  satoshis. No exchange-rate conversion combines these limits.
- No watchtower, or one selected discovered provider public key and a maximum
  1–1,000 bps rescue rate. Every new action requires a fresh signed proof within
  the chosen policy. There is no upfront charge.

The initial eligible check is immediate after a new enabled policy is saved.
Edits delay future checks by at least the cadence. Fixed-rate policies renew
expired/finished offers; they do not churn an otherwise live quote. Edits apply
to future offers, retaining every existing signed/accepted term. An additional
open slot may use fresh funds while earlier swaps settle.

## Optional reference

The default is a fixed rate. The optional orderbook reference requires an
explicit list of 3–16 distinct external maker hex public keys. Own quotes are
excluded. Each chosen identity must have a valid signed, open, same-direction
offer within the configured 30–300 second freshness window, and every configured
relay must have a complete fresh view. Each maker contributes its most recent
qualifying offer, with deterministic event-ID tie breaking.

The daemon uses the exact median ratio only if the range between the lowest and
highest maker rate is within the configured 1–100 bps spread. Sparse, stale,
future-dated, invalid or conflicting evidence pauses the policy. There is no
assumed BLAKE price oracle. Selected identities can collude or be controlled by
one person; their signatures establish provenance, not independent fair value.
The UI exposes reference event IDs and the last decision. Hard authorized price
bounds apply even when every selected maker agrees.

Repricing uses a new signed ID through the same atomic cancel-and-replace path
as manual order management. A live source must be unreserved and its previous
publication acknowledged before repricing. Failed relay publication retains the
durable outbox and reports publication pending. It does not authorize duplicate
successors or change the maker's acceptance authority.

## Durable budget accounting

Saving an offer reserves its sell principal and separate per-chain allowances
in the same encrypted snapshot as the offer, coin reservation, lineage and
accepted confirmation receipt. The paid-chain allowance is its selected funding
fee plus 20,000 settlement sats and the maximum authorized rescue bounty on its
principal. The received-chain allowance is 20,000 settlement sats and its maximum
rescue bounty. These conservative limits cover owner/tower outcomes without
assuming a particular settlement path. They are authorization accounting, not a
statement that fees or bounties were paid.

When maker funding has been signed, its reserved charge becomes permanently
committed. A refund, completion, reorg or unavailable chain response never
restores that allowance. Only a conclusively unfunded local cancelled/expired
intention releases its reservation. Missing reserved swaps or missing historical
authority remain uncertain and retain their allowance. A replacement transfers
only its own uncommitted allowance and coin reservation; it cannot reclaim a
prior funded trade's budget. Lower limits cannot erase commitments/reservations.

Each attempt rechecks exact source authority, spendable confirmed coins, BTC
replay eligibility, fees, provider proof, reference and remaining policy budget.
The persisted pending confirmation identity survives restart. A successor,
charge, receipt and next cadence are saved atomically. At most one eligible
policy action runs per daemon tick, and the next attempt is scheduled from the
current time before I/O. Missed intervals are never replayed as catch-up offers.

Disabling first durably prevents future actions. It can optionally cancel still
open, unreserved policy offers, with relay acknowledgement reported separately.
It cannot cancel a funded swap or delete signatures, obligations or accounting.
Restart preserves disabled state, consumed allowances and successor IDs.

Both portable and legacy imports disable every policy before installation and
quarantine old live offers and queued publications. Old pending automatic grants
are revoked while their receipt IDs and digests remain durable; accepted receipts
and funded obligations retain their original meaning. An old snapshot can omit
later spending. Imported reserved charges therefore remain uncertain and continue
counting against the limits, even after explicit reauthorization. They cannot be
released by expiry or transferred to a replacement. Recorded committed charges
remain permanent.

Chain recovery readiness alone never resumes an imported policy. After current
recovery completes, review its complete limits for the current profile and
explicitly acknowledge potentially omitted spending when enabling. Saving or
disabling a policy retains the import hold. A newly authorized action uses fresh
funds and a new request/offer ID. Re-importing holds policies again and preserves
all accounting, receipt and successor identities. Backups and future archives
must retain these facts. Import and daemon startup reject malformed policy or
charge records before installing or executing them; accounting is never repaired
by silently dropping records. Routine cadence/decision/reference polling stays
in the full archive but does not mark a backup stale. Policy authorization,
holds, pending receipts, charges, successors and actual offer actions do.

Current capacities are 32 policies per wallet and the existing durable
order/confirmation history limits. Capacity exhaustion pauses with a visible
decision. Existing settlement continues independently of automation failures.

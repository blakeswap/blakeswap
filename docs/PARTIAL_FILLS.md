# Partial-fill protocol and state design

This document specifies the T13 implementation. It is a design contract, not a
record of completed tests or an independent security assessment. Implementation
and validation results are recorded separately in `TESTING.md`.

## Cutover and identities

The unreleased application makes a hard cutover to protocol format 2 and vault
state format 3. A missing or older marker is incompatible, not a default. The
public offer, request, immutable terms, private message, tower advertisement, job
and receipt boundaries all identify the upgraded format. The network namespace
is `blakeswap-<network>-v2`; chain genesis and replay-signature domains do not
change. The typed service and HTTP API use v2. There is no version negotiation,
old execution branch or automatic migration of development state.

Only an absent vault initializes State3. Every authenticated existing-state
reader must reject older state before activation, including engine startup,
offline action/network readers, archive snapshots and complete/streamed backup
validation. Portable encryption envelope versions remain separate from the
embedded state version. Rejecting old state must preserve the source; a user
may explicitly create a separate development profile. The application never
silently resets or deletes a rejected profile.

Existing-file preflight opens the encrypted vault read-only with a one-second
writer-lock timeout and authenticates its state before any source cleanup or
write. Activation opens the existing file without creation, empty-file
initialization or an opening freelist transaction, then repeats validation under
exclusive writer ownership. Both passes inspect protocol-bearing cold records
in bounded pages, without reconstructing unrelated lifetime history. A cold
core and its required companions are format-checked before any of them acquire
active ownership. First
initialization writes a complete current-format state in a private sibling
directory and atomically links it into an absent final path. Failed writes or
interruption before publication leave that final path absent; interruption after
publication leaves complete State3. A competing existing destination is never
overwritten. An arbitrary preexisting empty database is still incompatible.

After T10 integration, credential acquisition authenticates and checks the
existing state format **before** the credential migration journal begins or a
new Keychain item is created. Activation repeats identity/format verification.
Adding a check only to the post-creation verifier would not preserve the
untouched-source contract. Snapshot and verifier credentials remain owned byte
copies, with the existing cancellation/cleanup lifetime.

An order is a signed parent. Immutable economics comprise format, network,
parent ID, maker, sell asset, total sell amount A, price numerator B (denominator
A), `ceil_buy` rounding, whole/partial mode, minimum and maximum sell quantity,
and expiry. Editing economics creates a new parent; accepted child terms never
change. Mutable signed availability comprises revision, available amount, and
open/cancelled/closed status. Private protection choices remain private.

A request contains format, a unique child ID, the complete exact signed parent
event, its revision, requested sell quantity q, taker identity, secret hash, and
two distinct per-chain keys. The child buy quantity is derived, never supplied
as an independent unbound price. Accepted terms commit the exact request,
derived amounts, both parties' keys, contracts, chain domains and deadlines.
Funding messages and rescue jobs/receipts bind the resulting child TermsHash
and exact funding outpoints. There is no shared HTLC or parent secret.

Each local child generates a fresh secret and derives keys by child identity,
chain and role. Retained active/cold child identities reject reuse of a known
sibling's hash or keys. Exact retries are exempt only when the complete request
matches. A sibling secret, funding transaction, rescue signature or receipt
cannot substitute for the requested child.

## Quantity ownership

All quantities use integer units of the maker's sell asset:

```
Total = Available + Reserved + Committed + Filled + Released
```

Every bin is nonnegative. `Released` is the current amount withdrawn from this
parent, not a cumulative counter. An optional historical-return counter is
audit information and never enters this equation. The parent ledger, exact
child request/disposition, coin and fee assignments, and outgoing acceptance
are persisted together before an acceptance can leave the wallet.

Each child retains immutable original q. Its current allocation is q in exactly
one child-owned bin, or **zero** after an irreversible unfunded return to
Available. Such a child is `retired`: its original quantity stays in audit
history but no longer participates in the parent's current child sum. This
prevents a later child from double-owning returned inventory.

| Trigger | Transfer | Required durable evidence |
| --- | --- | --- |
| Create parent | total to Available | Explicit review, valid economics and resource authorization |
| Accept a new child | Available to Reserved | Exact current revision, valid q, disjoint inputs and fee allocation, child terms and acceptance saved |
| Save own funding bytes | Reserved to Committed | Transition occurs before any possible broadcast, including an ambiguous attempt |
| Positive completion | Committed to Filled | Exact current funding and settlement observations for this child |
| Positive funded refund | Committed to Released | Current settled outcome; no automatic reoffer of returned coins |
| Contradict a recorded settlement | Child Filled/Released to Committed | Durable child-specific chain contradiction; funding descendants also acquire proof holds |
| Safe unfunded failure, open parent | Reserved to Available | Irreversible no-funding decision; child becomes retired with zero current allocation |
| Safe unfunded failure, closed parent | Reserved to Released | Same proof, but no return to an open pool |
| Cancel/expire parent | Available to Released | Reserved and Committed obligations remain intact |

A safe unfunded return requires this installation's durable knowledge that no
own funding bytes, variant or transaction identity have ever existed, together
with an irreversible local refusal of any future funding after the funding gate
has positively closed. Wall-clock expiry, absence, an unavailable indexer or a
missing peer does not suffice. Imported uncertainty cannot assert never-signed.
If funding could have been published, retain Committed until positive outcome.

Retired children retain authenticated request identity and monitoring knowledge.
An exact retry receives a closed result and cannot enqueue a new acceptance.
Changed content under an existing child ID is rejected before any quantity
change. Late incoming funding is recorded for that original child and safe
unwind; it cannot restore allocation, authorize maker funding or revive a
removed acceptance publisher. Unknown outcomes and irreversible secret knowledge
survive restart, archive and recovery.

Parent cancellation withdraws only Available and unassigned inputs/fee reserve.
Remainder-only replacement keeps the sell asset and uses exactly that available
quantity. It selects only the old unassigned input pool and transfers no more
than the unused per-asset authorization into the freshly reviewed parent. Each
old budget retains its original limit, Reserved and Consumed values and records
Transferred separately, so transferred permission cannot be reserved again. A
replacement that needs unrelated funds or larger authorization requires a
separate new-order review. The old
parent and all accepted children, permanent charges, receipt identities and
uncertain funding remain. Replacement cannot cancel a funded child by deleting
its parent or reuse its input assignment.

Restored parents retain the exact bins and fee/receipt facts under a restore
hold. Historical Available does not resume a publisher or become a new signing
grant. Explicit recreation uses fresh reviewed funds and preserves old unknown
obligations. Neither advisory history inclusion nor another completed child
releases a may-published slice or credits T11/T12 authorization.

## Rounding and quantity admissibility

For requested sell q, compute `b = ceil(q * B / A)` with exact wide integer
intermediates, checking the supported result before converting to int64. This
preserves `b*A >= q*B` for every child and therefore for their aggregate. A/B
are a price ratio; B is not an aggregate rounded-payment ceiling. Independent
ceilings can add less than one buy unit per child, and the review shows that
child's exact rounded amount and effective rate. No price depends on the order
of settlement, a preceding child's return or a reorg.

Whole mode permits only q=A. Partial mode requires an explicit min/max within
the existing per-leg amount bounds. The legal protocol interval is the
intersection of signed sell bounds and the inverse of the monotone rounded-buy
amount bounds. With its resulting minimum m and maximum M, a remainder r is
partitionable exactly when r=0 or `ceil(r/M) <= floor(r/m)`. Apply this to the
initial total and every proposed available remainder. There is no below-minimum
last-fill exception.

This interval proves protocol quantity feasibility only. Actual input sets,
transaction weight, fees, selected protection and current funds are checked
separately for the chosen child. They are not assumed to form a contiguous
quantity interval. Each child also satisfies the actual non-dust owner/refund
payout and tower-bounty constraints after rounding. A fee/resource change can
pause new admission or require new review; it never alters agreed contracts.

## Inputs, fee authorization and dependencies

The parent owns confirmed wallet outpoints, not fractional claims against one
coin. Acceptance atomically transfers complete selected outpoints from the
parent pool to that child. Concurrent children require disjoint inputs. A wallet
with one large input may accept one child and wait for confirmed change; the UI
distinguishes advertised quantity from immediately fundable concurrency. No
automatic coin-splitting transaction is part of this design. A failed selection
changes no quantity, receipt or resource ownership.

Funding consumes the exact assigned inputs and revalidates source freshness,
ownership, replay readiness and the reviewed fee ceiling before signing. Parent
resource reconciliation cannot reclaim a child's signed or uncertain inputs.
Only positively confirmed unspent change can support another child.

Confirmed change creates a real funding dependency: if A's funding change pays
for B, B is A's descendant. A reorg of A must invalidate B's dependent proofs
and monitoring, preserving both allocations and all permanent charges until
their respective evidence resolves. The independence guarantee applies to
children with unrelated funding ancestry, not descendants. It never erases a
known secret or changes either child's immutable contracts.

The local maker and taker both retain direct `FundingParents` edges in signed
input order before publishing funding. Each edge binds the producing local child,
chain, exact funding transaction and change output. Its `funding/<chain>/<txid>`
identity index moves atomically with the producing child into encrypted cold
storage. Validation point-reads direct parents and checks both the core-to-index
and index-to-core binding. It never materializes the lifetime ancestor graph.

A dependent child requires its own current confirmed funding inclusion before
first revelation, refund, positive settlement or deep archival. That inclusion
proves its transaction ancestors under the selected backend's consensus view.
Each child has an independent bounded check of source generation, captured tip,
transaction identity and canonical block; an earlier failed child cannot consume
another child's proof allowance. Unknown evidence persists an explicit monitoring
hold. A positive contradiction of the child's saved funding inclusion returns a
maker's Filled/Released allocation to Committed, preserving consumed charges and
never returning inventory to Available. Fresh positive evidence can clear the
hold and reconcile that child's outcome again.

A saved hold is not a new authorization. Reopen, import and cold activation require
fresh proof. The original saved first funding publication and exact authorized
retry retain their existing guards, avoiding a circular wait for their own
confirmation. An already public secret can still drive the existing incoming
claim rescue. Neither exception authorizes a new funding template, first secret
revelation or refund while ancestry remains uncertain. Cold queries preserve the
monitoring status without promoting ancestors or publisher authority.

Every child reserves its own funding and settlement/refund fee plan and optional
tower payout. Parent authorization has hard per-asset monetary fee/bounty caps;
a maximum accepted-fill count can additionally bound activity but cannot replace
those caps. Derivation and aggregate count-times-fee calculations use checked
wide intermediates. Extra wallet-input costs and fees deducted from HTLC outputs
remain separate. Reserved and permanently consumed charge facts survive
refunds, reorgs, export/import and explicit reauthorization.

T11 and T12 create explicit upgraded whole-mode parents until partial automation
is itself reviewed. Their permanent spending/fee charges do not reset. Manual
partial-order admission binds its own parent policy and child allocations to
the exact T10 consent receipt; no full-parent charge is duplicated per child.

## Relay publication and concurrency

Nostr replacement ordering remains `(created_at, lowest event ID)` for each
address. A payload revision never overrides it. Each own parent has a durable
per-address publication clock and pending revision. Sign at most one revision
per wall-clock second for that address, and never invent a future timestamp.
When the ledger changes before the next timestamp is usable, coalesce the
pending availability view and **hold new takes** until the current revision has
its signed event. Existing accepted children and settlement keep progressing.

Cancellation/expiry takes effect locally at once; its publication may wait for
the next timestamp. Requests still require the exact current signed event and
revision and an open ledger, so an old relay view cannot acquire removed
inventory. Racing takes of one event serialize: one may reserve, the other
receives stale revision and needs a fresh quote/consent. This cadence limits new
admission per parent, not concurrent settlement across already accepted children.
Clock rollback holds publication/admission instead of advancing a global future
counter. Cold own ordering floors and pending state survive restart.

Remote equal-time conflicts use normal Nostr ID ordering. Conflicting content
or regression of a known same-parent revision cannot become local execution
authority; a signed availability revision does not change immutable economics.
Local exact retries resolve retained child identity before current revision and
expiry checks. A retired retry is closed; an accepted retry can repeat only its
original immutable acceptance.

## Storage, API and validation boundaries

Parent bins and child allocation/fee identity are core recovery state. T09
atomic archive moves and direct/reorg activation must include their authority
companions before any child advances. Retired body history may be cold, with
bounded active aggregate metadata, but a location change cannot change bins,
lose exact duplicate responses or resume old publication. Streamed restore
cross-checks parent/child ownership before installation and holds publishers.

Completed-state validation joins exact active and archived parent, child, core
and fee companions. It reconciles every child-owned bin, parent withdrawals,
reserved and permanent per-asset charges, immutable accepted terms and conflicting
live input assignments. Single streamed rows receive structural validation only;
the authoritative graph check runs after all records arrive and before the private
copy becomes installable. Existing-vault preflight and activation repeat the same
check without repairing a rejected source. Encrypted external sorting bounds
working memory rather than assembling lifetime child arrays. Standalone checks
retain no input-owner collection. Live engines build an independently keyed,
encrypted disposable exact-key input index in bounded batches, including cold
unresolved owners. Ordinary saves use previously validated parent totals plus
exact changed-child contributions and point reads for touched cold companions;
they stage affected index changes until the authoritative vault commit succeeds.
They do not scan or copy the cold ownership population on each tick. Closing the
engine or its vault removes the index; restart rebuilds it from authenticated
custody. Validation does not establish chain finality or clear imported holds.

The quote/confirmation API and native review bind wallet, network, signed parent
event/revision, q, derived b, min/max/remaining, effective rate, net proceeds and
per-child costs. A stale asynchronous response or persisted confirmation retry
cannot authorize a different quantity. Parent history links all child outcomes;
a single child completion does not mark the entire parent filled. Market rows
show the local conserved bins separately from the signed availability event and
its publication or restore hold. Partial child history is paged through
`fills.list`; it is not accumulated in a lifetime array inside a hot parent.
Whole-mode final history retains its one currently allocated terminal child
link, including when both parent and child are cold. A still-open remote parent
remains available even when this wallet has another pending child. Suggested
quantities satisfy the signed bounds and remainder rule; confirmation still
uses the exact reviewed quantity.

Required validation includes arithmetic properties and overflow, conservation
through returns/cancellation/reorg, racing requests, save failure and restart
before/after acceptance, exact and changed retries, real input isolation and
descendants, fee changes/caps, secret/funding/tower substitution, late funding,
restore uncertainty and source-preserving legacy rejection. Actual private BTC
and Blake2b cases cover both directions, RPC and Electrum, protection on/off,
concurrent independent children and mixed completion/refund. Focused independent
protocol/security code review precedes the final whole-PR review. Neither this
design nor deterministic fixtures alone claim real-fund readiness.

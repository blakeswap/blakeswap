# Partial-fill API and native implementation contract

This is the shared T13 implementation interface. The declarations are committed
before the API/native implementation so those changes can proceed independently
of daemon state-machine wiring. They are not a claim that partial fills are
already callable. Before delivery, move the complete typed service/package and
all HTTP routes from v1 to v2, regenerate Go, gateway, OpenAPI and Swift through
`scripts/generate-api.sh` and `scripts/generate-swift.sh`, update every consumer,
and remove v1 registration. The current declaration path is transitional; there
will be no v1 compatibility handler or default legacy wire execution.

## Create and review an order

`CreateOfferRequest` and create/replace/recreate `TradeQuoteRequest` carry
`fill_mode` (`whole` or `partial`), `min_fill`, `max_fill`, `fee_budgets` and
`bounty_budgets`. Both map keys are asset IDs (`btc`, `blake`); amounts are exact
integer smallest units of that asset. Each accepted creation freezes explicit
hard per-asset limits. A missing map or entry is not unlimited authorization;
zero bounty is valid only for an unprotected leg. A whole order explicitly uses
min=max=the total sell amount. T11/T12 create explicit whole orders and retain
all permanent charge accounting.

Public `Offer` contains the protocol version, explicit mode/bounds, total A/B,
revision and current available quantity. The old public reservation field is
removed. Private fee/bounty budgets and protection choices never enter the
signed public offer. Parent immutable economics remain those in
`PARTIAL_FILLS.md`; availability updates do not edit them.

`TradeQuote` keeps its existing opaque token/revision and adds the mode, bounds,
available quantity, parent revision, total sell amount, aggregate funding
reserve and frozen per-asset caps. For creation, `paid_principal` is parent A,
`paid_total` is A plus the aggregate extra funding reserve, and
`received_principal` is price-reference B. In partial mode B must be labelled as
the quoted rate's reference amount, not a promise that independently rounded
children sum to exactly B. The aggregate fee/bounty authorizations are displayed
separately from coin principal and the extra funding reserve.

For partial creation, `example_fill` is a separately labelled representative
child with exact sell/buy quantities, funding fee, owner fee cap and per-child
outcomes. The top-level outcomes are empty in this case; displaying example
outcomes as aggregate parent results is incorrect. Whole creation keeps the
existing single-trade outcomes. The representative quantity must pass both the
economic tail check and the current fee/net-output checks. It does not grant
permission to change a later taker's explicit quantity.

## Take exactly the reviewed quantity

`TakeOfferRequest` and take `TradeQuoteRequest` require `quantity` in the maker's
sell asset and `parent_revision` from the exact signed event. The buy amount is
`ceil(quantity*B/A)` and is derived by the daemon. For a take quote, all existing
paid/received/outcome fields describe that exact child, `quantity` is the exact
request, and `example_fill` is absent. The private fee/bounty caps in the quote
are the taker's exact one-child authorization, not the remote maker's limits.

`MarketOrder.suggested_quantity` is a legal economic default for the current
available amount, or zero when none is available. It is not a claim that the
selected wallet currently has a usable UTXO/fee plan. The client may use it to
initialize an unedited quantity field; it must never overwrite user input or
quietly adjust quantity during quote refresh/confirmation. For bounds
400000–600000, a total of 900000 suggests 400000 (600000 leaves an invalid 300000), while
a total of 1100000 suggests 500000 (400000 leaves an invalid 700000).

Refreshing a stale parent requires a new review. ConfirmTrade retains the same
opaque token/revision/request-ID/wallet/network contract; the durable quote
snapshot now includes exact quantity, parent revision, mode/bounds and monetary
caps. Confirmation cannot substitute a newer parent revision or suggested
quantity. An uncertain exact receipt retry returns its existing accepted result,
including through authenticated cold lookup, without a second reservation,
child, consent grant or fresh signature. Rejected/retired child identities do
not become a new take. T10's exact receipt exception is preserved only for that
same immutable request and result.

## Parent and child history

A parent identity is `(network, maker, parent_id)`, not just parent ID. Each
`PublicSwap` carries parent maker/ID/revision and original sell quantity.
`allocation`, `allocated_quantity` and `allocation_known` expose only locally
established maker accounting. A retired child retains original quantity for
audit but has known current allocation zero. A taker cannot infer the remote
maker's current quantity bin from its own cached swap stage; unknown allocation
has an empty disposition and `allocation_known=false`.

`MarketOrder.quantities` is present only for a locally owned maker parent and
projects the durable conserved total/available/reserved/committed/filled/released
bins. It is never recomputed by summing the currently visible child page.
Public available quantity remains distinct from private bins and from whether
current funds/proofs permit another acceptance.

`ListFills(FillQuery)` maps to the read-only daemon command `fills.list` and
returns `FillPage`. The request requires exact selected wallet/network and
parent maker/ID. Empty revision starts a frozen result; subsequent pages use the
returned revision. Offset/limit follow the existing bounded history conventions
(default 100, maximum 500), with deterministic original child-ID ordering, complete
count, next offset and more flag. Only local retained children are returned;
this is not a relay-wide list of another maker's fills. Active and cold records
share one result and context/source fences. No core is reactivated to list it.
Each row links to the existing `record.get(kind=swap,id=child)` route.

The parent row may retain a bounded convenience list of child IDs, but complete
parent history must use ListFills; it must not allocate a lifetime child array
inside each market row. Native parent details show the conserved summary and
page children separately, keeping exact navigation and archive/monitoring state.

## File ownership for the parallel implementation

The protocol/daemon lane owns `internal/protocol`, `internal/transport`,
`internal/chain` protocol changes, `internal/daemon`, `internal/storage`, semantic
backup/recovery/version-preflight changes in `internal/desktop`, actual-node
matrices, and protocol/operations/testing design. This lane implements all new
DTO production, admission, exact quote snapshots, `fills.list` and conservation.

The API/native lane owns the complete v2 schema/package move, generated Go and
Swift/OpenAPI, `internal/api` mapping/service/tests, generator scripts, native
market/trade/parent-child views and models, and native consent/retry tests. It may
make mechanical generated-package import/path edits in desktop/test consumers;
coordinate semantic desktop/provider changes with the daemon lane. It must keep
T10 credential byte ownership, structured consent and exact retry exceptions.

The API/native lane starts from an agreed clean T13 source checkpoint in its own
worktree after PR22 is verified merged. No shared worktree edits. The final T13
integration includes both lanes in one PR, full current protocol/security and
whole-PR review, generation checks and real both-chain acceptance matrices.

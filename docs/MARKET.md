# Market browsing and order management

The Market view compares whole, fixed-size BTC ↔ BLAKE offers. The side always
means the selected wallet's intent: **You buy BTC** or **You sell BTC**. Switching
between My orders and Other makers does not reverse that filter. Rates are BLAKE
per 1 BTC; both assets use 100 million satoshis per coin. Display rounds to eight
decimals, while sorting compares the exact integer ratio using arbitrary-precision
cross-products. Equal prices use maker key and order ID as deterministic ties.
Size means BTC principal, including when the maker sells BLAKE. Amount filters
accept whole BTC satoshis, and expiry sorting uses the signed expiration time.

The maker decides whether an offer is available when it receives a request.
Relays do not constitute a matching engine or prove funds. A successful relay
read timestamps the local view; after two minutes without one, other makers'
open offers appear stale. The UI refreshes its local view every 15 seconds and
provides Refresh market to request a new daemon tick. Partial relay availability
is shown separately. An open offer already requested by this wallet appears
pending and cannot start a second request. The daemon preserves its existing
maker-authoritative exact-event, request-deduplication, and funding checks.

## Your orders

My orders includes open, pending/reserved, filled, cancelled, expired, and refunded
outcomes. Own signed offer records and their management metadata are stored in
the encrypted wallet independently of the expiring public book. Activity links
remain available after restart or expiry. Details links an order to its swaps,
shows its replacement/recreation lineage, and distinguishes an unknown legacy
creation time from a known local creation time. A locally terminal refunded swap
can establish a refunded outcome even when the retained signed offer still says
reserved. This does not rewrite the accepted terms.

Publication has three distinct meanings:

- **Saved locally; relay publication pending:** the state and signed event are
  durable, with a retry queued. This includes a just-committed cancellation.
- **Relay storage acknowledged:** every configured relay acknowledged the current
  event during a completed publication attempt. This is storage evidence, not a
  guarantee that other users have received it or that it will remain available.
- **Prior relay publication unknown:** legacy state has no durable ACK evidence.

With no configured relays, an event stays queued; absence of destinations never
counts as acknowledgement. A new signed version resets its publication evidence.
After replacement, inspect the old cancelled row to see cancellation publication
and the new row to see the replacement publication.

## Expiry, cancellation, and replacement

Create offer accepts a custom expiration up to seven days ahead. The economics
review includes the exact expiry. The daemon rejects expired or excessive dates
at both review and confirmation. Expiry closes an unsigned offer; it cannot undo
an accepted or funded swap's settlement obligation.

Cancel applies only to an unreserved own offer. Its request carries the wallet,
network, and exact event ID shown in the view. Retrying that same cancellation
returns the same cancelled order. A stale relay copy cannot authorize a maker
acceptance after local cancellation, even before relay publication completes.

Edit / replace opens a fresh economics review, including current funding inputs,
fees, expiry, and optional provider selection. Confirmation creates a new order
ID and cancels the old one; it never changes accepted signed terms in place. The
review can consider the original order's reserved coins without releasing them.
Ordinary preflight and send requests still see those coins as locked.

The engine serializes acceptance, cancellation, and the final replacement. Both
public events are signed first, then cancellation, new order, reservation transfer,
lineage, and the accepted confirmation receipt are saved in one vault transaction
before either event can be sent. An interrupted save leaves the prior durable
order and pending confirmation. A retry after restart uses the same authorized
request ID, including after quote expiry, rather than creating another offer.
If another taker wins the race, replacement fails and the accepted swap retains
its reservation. An ambiguous response should be resolved with Retry saved
confirmation, not by submitting a different replacement request.

Recreate is deliberate: a finished, cancelled, or expired order supplies initial
form values for a **new** order. The new ID, funds, fees, provider proof, and terms
must pass a fresh review. A reserved order requires a linked terminal local maker
swap before recreation is available. Automatic renewal and repricing are not
performed. Existing capacity limits still apply to retained orders.

## API

`ListMarket` / `market.list` / `POST /v1/market/query` accepts:

```json
{"expected_wallet":"alice","expected_network":"regtest","owner":"mine","side":"buy_btc","status":"all","btc_min":"100000","btc_max":"10000000000","sort":"rate","descending":false,"offset":0,"limit":100}
```

Owner is `all`, `mine`, or `others`; side is `all`, `buy_btc`, or `sell_btc`; status
is `all`, `open`, `pending`, `reserved`, `filled`, `cancelled`, `expired`, or
`refunded`. Zero amount bounds are unbounded. Limit defaults to 100 and is at most
500. A response includes a revision of the complete filtered, sorted result set.
Send that revision with a nonzero offset; a changed set rejects the next page so
the client can refresh rather than skip or duplicate rows. No chain or relay I/O
occurs in this read. Desktop reads use the cancellable advisory lifecycle route.

For replacement/recreation, use `QuoteTrade` with `kind: "maker"`,
`order_action: "replace"` or `"recreate"`, `source_offer_id`, and the exact
`source_event_id`, alongside the ordinary new draft/fee/provider fields. The
response echoes those bindings, and its token/revision are confirmed through
`ConfirmTrade` with a durable request identity. Direct `CreateOffer` cannot bypass
this review for management actions. `QuoteFee` permits `source_offer_id` and
`source_event_id` only for a funding quote bound to `expected_wallet`; this is the
same validated reservation allowance, not permission for general coin reuse.

`CancelOffer` accepts `expected_wallet` and `expected_event_id` in addition to the
existing required desktop `expected_network`. Older direct clients may omit the
new optional bindings; native management always includes them. Money remains
exact integer satoshis in the protobuf contract and decimal strings in protobuf
JSON. Responses omit other makers' private provider choices.

# Activity history and export

Activity combines received payments, sends, own orders, swaps, funding, claims,
refunds, and locally earned tower bounties. It is stored with the encrypted wallet
snapshot. Stable record and group IDs connect each payment to its order, swap,
send, outpoints, and retained signed transaction variants. Reopening the wallet
or replaying messages updates these records rather than adding another payment.

## Amounts and related records

Amounts are integer satoshis of the named asset. BTC and BLAKE are never combined.
`movement` distinguishes an economic movement from a related lifecycle or receipt
record. Movement rows use these conventions:

| Kind | Direction and amount | Fee treatment |
| --- | --- | --- |
| External receipt | Incoming owned output value | Sender's fee is unknown |
| Send | Outgoing recipient value plus mining fee | Exact retained signed variant |
| Send to a known wallet address | Internal; amount is the mining fee | Principal remains in the wallet |
| Swap funding | Outgoing contract principal plus mining fee | Persisted funding authorization |
| Claim/refund | Incoming owner payout | Mining fee and tower bounty are separate fields |
| Tower rescue | Incoming tower bounty | Contract owner pays the mining fee |

Order and swap lifecycle rows are informational (`movement=false`). Related
funding and settlement rows describe the asset movements. Indexed change, swap
payouts, tower payouts, and known self-transfer receipts link to their parent and
do not count that payout again. If a transaction spends known wallet outputs but
ownership of all inputs/outputs is unavailable, its receipt is explicitly
`unclassified_self_related`, not a new external deposit. Other receipts remain
`unclassified_receipt`. Unknown fees and unrelated external send authorizations
are not invented.

## Time, confirmations, and reorgs

Local creation time, first local recording time, block time, and observation time
are separate fields. Retained records without creation timestamps keep
`created_time_source=unknown` and an empty creation time in CSV. Backfilling an old
deposit today does not claim that it was received today. Newest-first ordering
uses known creation time, otherwise block time, otherwise first recording time;
stable IDs break ties. Date filters use the same ordering time.
Prior local lifecycle outcomes use their local update time with source
`local_state`; this does not manufacture a historical creation or block time.

Chain status can move from confirmed back to confirming, mempool, orphaned,
conflicted, or unknown. Current rows follow observations; prior outcomes remain
in history. An earlier replacement may confirm again after a reorg, restoring
that variant's exact amount and fee. Absence or a failed server read alone is not
eviction evidence. `orphaned` requires an observed change to the previously
recorded block at its height. Observations older than two minutes become unknown
when reconciled. Protocol secret knowledge and recovery obligations remain
independent of this advisory ledger. Sources are opaque endpoint fingerprints,
not URLs or credentials, with generations when supplied by the chain backend.

## Historical coverage and limits

The index walks every retained receive address, including rotated addresses and
spent outputs. RPC uses watch-wallet receipt history and the transaction index;
the node must retain those transactions (`txindex=1` and suitable watch-wallet
imports/history). Electrum script history is checked against raw transactions
and confirmed inclusion against the chain's headers. Unsupported history reads
produce a warning. Current UTXOs are only an initial unverified projection, not
a claim of complete deposit history.

Indexing runs after settlement work, outside the engine mutex. Each chain gets a
200 ms history page (two transactions) and a 200 ms observation pass (at most
eight variants). Reads cancel and join when the engine closes; late replies
cannot cross wallet/network/source generations. A completed pass starts again
to discover arrivals before its previous transaction-ID cursor. Errors preserve
evidence and retry, with coverage warnings in Activity.

Limits are 50,000 records, 10,000 history transaction IDs per address, and 2,048
inputs/outputs per indexed transaction. Exceeding a limit leaves an explicit
incomplete-history warning and preserves existing records. The cursor does not
advance past a transaction that could not be indexed. Existing protocol send,
receive-address, and message capacities still apply. There is no production
archive/compaction workflow. A mnemonic cannot restore local lifecycle times or
prior observations omitted by a provider or old backup.

## Query, navigation, and CSV

The native Activity page filters by type, status, asset, and date. Refresh starts
a new snapshot; Load more continues the same frozen newest-first snapshot while
new activity arrives. Details show amounts, provenance, related IDs, variants and
prior outcomes, with navigation to the order, swap, or send.

`ListActivity` (`activity.list`, `POST /v1/activity/query`) requires
`expected_wallet` and `expected_network`. Optional fields are `kind`, `status`,
`chain`, `from`, `to`, and `limit` (1–500; default 100). Continue with returned
`snapshot` and `next_cursor` as `cursor`, preserving filters. Zero `next_cursor`
means complete. Snapshots expire after ten minutes; four are retained with
oldest-first eviction. Expired/replaced snapshots or changed scope fail explicitly
and require refreshing the first page.

`ExportActivity` (`activity.export`, `POST /v1/activity/export`) takes the same
query. CSV chunks share its frozen scope and include the header only on the first
chunk. Continue until `next_cursor=0`. Native export fetches all selected rows,
not just the loaded page, before writing the file atomically. Wallet, network, or
filter changes discard an in-flight export.

CSV includes exact integer satoshis, separate assets, stable IDs, fee-known flags,
UTC timestamps/provenance, classifications, variants, and prior outcomes. Quotes,
commas, and newlines use standard CSV escaping. Formula-like text is prefixed
with an apostrophe. Unknown fees/times stay blank; numeric columns never use
floating point. Mnemonics, keys, preimages, raw transactions, credentials, and
private messages are absent from both projection and export.

Explorer links are optional per chain/environment in Settings. A transaction URL
template must contain exactly one `{txid}`. HTTPS is required except HTTP loopback
on regtest; credentials, queries and fragments are rejected. No explorer is
guessed. Opening a link sends that transaction ID to the configured explorer.

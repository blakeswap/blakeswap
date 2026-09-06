# Daemon API

## Contract and transports

`api/proto/blakeswap/v1/daemon.proto` is the source of truth. It declares the
`blakeswap.v1.DaemonService` protobuf service, typed requests/responses, HTTP
bindings (`google.api.http`) and OpenAPI operation/security annotations. Generated
Go server/client/gateway bindings, Swift message/client bindings, and the OpenAPI
2.0 document are committed. HTTP and native clients invoke the same Go service
and wallet command implementation.

The native app uses **gRPC directly**, with gRPC Swift 2 over HTTP/2 on a private
Unix-domain socket. It never calls the HTTP gateway. SwiftProtobuf decodes binary
messages, preserving signed 64-bit satoshi values without floating-point JSON
conversion. The UI refreshes status approximately every 1.5 seconds. This version
uses unary RPCs; it does not claim to expose a separate trade WebSocket or streaming
RPC. Nostr's network transport uses WebSockets independently.

The **grpc-gateway HTTP API** is available for future clients on an ephemeral
`127.0.0.1` port. No wallet listener binds an external interface. Browser access
is deliberately not enabled yet: every `Origin` is rejected, there is no CORS,
and the Host must equal the literal bound address. A future web UI needs an
explicit same-origin serving and authorization design before relaxing this.

Each daemon start generates a fresh 256-bit bearer token. The Unix socket and
its `<socket>.json` endpoint file have mode 0600. Desktop discovery is the private
`runtime.json` in the app data directory, mapping profile to `{socket,http,token}`.
The desktop creates short socket paths in a private OS temporary directory to
avoid macOS Unix-socket path limits. These files are removed on orderly shutdown
and parent-death shutdown; hard-killing the daemon itself may leave stale runtime
files, which a later owned startup replaces.

Both transports require `authorization: Bearer <token>`. gRPC plaintext is used
only inside the local Unix socket. The HTTP gateway forwards that credential to
the actual gRPC server. HTTP body and gRPC request size are limited to 128 KiB;
responses to 8 MiB; RPC deadlines to 45 seconds. HTTP adds header/body timeouts,
no-store, and strict origin/Host checks. Tokens never appear in status or logs.
The token and file permissions do not protect against a compromised local user
account that can read those files.

## Methods

| RPC | HTTP | Purpose |
| --- | --- | --- |
| GetStatus | GET `/v1/status` | Public wallet identity, network, balances, coins, sends, chain heights, offers, swaps, delivery state, errors |
| RefreshStatus | POST `/v1/status/refresh` | Run a fresh wallet cycle and return its status |
| SetPaused | PUT `/v1/pause` | Compatibility endpoint: pausing is rejected; resume clears legacy state |
| ResolveWatchtower | POST `/v1/watchtowers/resolve` | Request an encrypted signed quote by npub or hex public key |
| CreateOffer | POST `/v1/offers` | Exact chain/amount pair, optional expiry, and maker-only private tower selection |
| CancelOffer | DELETE `/v1/offers/{id}` | Cancel an unreserved local offer |
| TakeOffer | POST `/v1/swaps` | Compatibility direct request for a signed maker offer |
| QuoteTrade | POST `/v1/trades/quote` | Read-only maker/taker economics, exact candidate funds, short-lived bound review |
| ConfirmTrade | POST `/v1/trades/confirm` | Revalidate one reviewed quote and durably authorize one offer/request identity |
| PreflightFunds | POST `/v1/wallet/preflight` | Fresh fee-inclusive candidate funds and BTC replay readiness; advisory only |
| SendCoins | POST `/v1/wallet/send` | Explicit coin selection, recipient, amount, total fee, and idempotent request ID |
| GetRecovery | POST `/v1/wallet/recovery` | Explicit sensitive recovery phrase request |
| BackupWallet | POST `/v1/wallet/backup` | Consistent encrypted state backup |
| Mine | POST `/v1/regtest/mine` | Test-node mining, regtest RPC only |
| Faucet | POST `/v1/regtest/faucet` | Test faucet to caller's deposit address, regtest RPC only |
| CreateWallet | POST `/v1/wallets` | Create an independent wallet using a name and Settings revision |
| GetSettings | GET `/v1/settings` | Desktop environment configuration and revision |
| UpdateSettings | PUT `/v1/settings` | Atomic compare-and-swap configuration update |
| PrepareFirstWallet | POST `/v1/onboarding/wallet` | Create or restore the first wallet with the current Settings revision |
| GetFirstWallet | POST `/v1/onboarding/recovery` | Explicit recovery phrase request while backup confirmation is pending |
| ConfirmFirstWallet | POST `/v1/onboarding/confirm` | Verify three requested recovery words and advance setup |
| ExportFirstWallet | POST `/v1/onboarding/backup` | Save a setup wallet backup with a chosen password and unused absolute filename |
| FinishOnboarding | POST `/v1/onboarding/finish` | Validate connections and mark setup complete using the current Settings revision |
| CheckNode | POST `/v1/settings/check-node` | Read-only chain identity/height check and trust description |

Settings methods belong to the desktop manager; standalone `daemon --config`
uses its configuration file and returns Unimplemented for Settings updates.
The authenticated HTTP `/openapi.json` endpoint serves the generated description.

Every amount is integer satoshis in its own chain; rates are integer basis points.
Protobuf JSON renders int64/uint64 as decimal strings. Clients should send strings
for monetary values and must not convert them through JavaScript `Number`.
The HTTP gateway emits snake_case names. The HTLC `refund_locktime` protobuf field
accepts the historical `refund_height` JSON alias for domain-state compatibility;
the value is a regtest height or a public-network Unix timestamp, as specified by
the associated network. The wire field number is stable.

Wallet mutations (create/cancel/take/send, watchtower lookup, pause, faucet, mine) carry an
`expected_network` field. The desktop rejects missing or mismatched values, so a
stale client cannot transact on a newly selected network. Native clients bind the
network shown in their status; the CLI snapshots it before dispatch unless the
caller supplies an explicit expectation. Standalone fixed-config daemons accept
legacy omitted expectations and reject mismatches.

A Settings update includes the latest `revision`; a stale write returns gRPC
Aborted/HTTP 409. Invalid configuration returns InvalidArgument. Active offers or
swaps block network switching, including when a broken endpoint prevents loading
the current wallet. Endpoint/relay changes within the current environment allow
reconnection and preserve signed terms and prepared transactions. Read-only
status/Settings snapshots remain available while an external service is slow.

## CLI compatibility

The CLI retains convenient method names and numeric JSON while internally using
the generated gRPC client. It is not the removed newline-JSON socket protocol.

```sh
bin/blakeswap call --socket /absolute/path/daemon.sock --method status
bin/blakeswap call --socket /absolute/path/daemon.sock --method offer.create \
  --params '{"sell":"btc","sell_amount":1000000,"buy_amount":2000000}'
```

There is no arbitrary signing, arbitrary PSBT execution, unauthenticated withdrawal, or
forced revocation of a funded HTLC method.

## Regeneration

Install `protoc` (the checked artifacts were generated with 3.21.12). Then run:

```sh
sh scripts/generate-api.sh
sh scripts/generate-swift.sh
```

The scripts pin the Go plugins and resolve exact direct Swift dependencies with a
committed `Package.resolved`. Google API and OpenAPI option protos and their
licenses are vendored under `api/third_party`. Regenerate all clients and the spec
together whenever the schema changes. Never reuse deleted protobuf field numbers.

The native client publishes wallet status and Settings as one snapshot only when
network and profile match. Profile changes and Settings saves invalidate earlier
responses, and older Settings revisions cannot overwrite a newer selection.

Status includes `funding_fee`, `own_watchtower` (including the copyable npub), and
`watchtowers` with verified payout scripts, fee, expiry, visibility and signed
announcement. Public-directory clients filter on `public`; private quotes can
still be selected by favorite identity. Each Settings environment has
`public_watchtower` (false by default) and `favorite_watchtowers` (npubs).
CreateOffer rechecks the confirmed sell balance against principal plus funding
fee and rejects expired discovery quotes. Each wallet retains its own provider's
signed quote in encrypted local protection state so settings edits cannot redirect
accepted rescue payouts. Public offers and shared terms omit those choices.

### Wallet profiles

Desktop Settings includes `wallets: [{id, name}]`. IDs are immutable storage
identities; names may be changed through `UpdateSettings` with its current
revision. Wallet removal, replacement IDs, and reordering are rejected. Use
`CreateWallet` (`POST /v1/wallets`, CLI `wallet.create`) with `name` and `revision`
to generate an independent encrypted seed and register a live endpoint in
`runtime.json`. Creation and renaming work with disconnected chain backends.
The desktop supports up to 20 wallets on each active network. Each wallet has its
own bearer credential, vault, network state, and npub. Changing networks checks
outstanding obligations in every saved wallet.

### First-launch setup

New desktop Settings starts with `onboarding_stage: "wallet"`. Preparation
accepts a name and either no recovery input (generate), `mnemonic` (BIP39), or
`backup_path` with `backup_password` (encrypted state restore). Generated and
phrase-restored wallets advance to `backup`; the response contains the recovery
phrase and three one-based `backup_word_positions`. Confirmation accepts the
three words in that order and advances to `connect`. Restoring an existing
encrypted backup goes directly to `connect`, retaining its network and swap state.
Only explicit setup recovery calls return the phrase; status and Settings never do.

Finishing accepts Settings still at `connect`, validates both active endpoints,
checks restored obligations before a network change, and clears the stage. A
missing/empty stage in pre-existing settings means setup is already complete.
Generic Settings updates and wallet creation cannot bypass a pending step.
All setup mutations carry a revision; stale calls are rejected. Preparation is
durable before advancing the stage, so a restart cannot silently replace keys.
Onboarding RPCs are unavailable after setup (use normal wallet recovery/backup).
The CLI names are `onboarding.prepare`, `onboarding.get`, `onboarding.confirm`,
`onboarding.export`, and `onboarding.finish`.

### Reviewed swaps

The native maker and taker forms use `QuoteTrade` (CLI `trade.quote`) before any
publish/request action. Supply `kind: maker|taker`, immutable `expected_wallet`,
`expected_network`, sell chain/principal and buy principal, an explicit
`funding_fee`, and `owner_fee_cap` (0 retains a fixed 2,000-sat owner fee; 20,000
authorizes the bounded owner ladder). Automatic funding estimates also carry
`rate_sat_kvb` and `fee_timestamp`. A taker includes the maker key and offer ID;
its displayed amounts must match that exact verified signed event. Protection
requires the selected `tower_pubkey` and its current `tower_bps` proof.

The result separates paid principal plus funding cost from received principal,
owner claim/refund ranges, and conditional tower outcomes in each chain's sats.
It includes an exact rational principal exchange rate, expected timing policy,
provider identity/coverage, and fresh fee-inclusive input/replay readiness. Quote
reads hold no funds, create no offer/request, and send no protocol message. A
wallet can retain up to 64 unexpired reviews; each expires within 120 seconds,
or earlier when its order/provider/automatic-fee review expires.

Persist a fresh 32-byte hex `request_id` before `ConfirmTrade` (CLI
`trade.confirm`), alongside the original `token`, `revision`, wallet and network.
The daemon rechecks signed order/proof, expiry, wallet identity, fee bounds and
fresh exact inputs before committing. A changed quote needs a new review. One
quote can authorize only one request ID. `accepted` means an offer is saved for
publication or a take request is saved for delivery; it does not mean funding or
settlement has completed. Status tracks the automatic sequence afterward.

Confirmation receipts live in the encrypted wallet snapshot. An exact accepted
or rejected retry returns the same result after expiry, order changes or restart;
reusing the ID with changed bindings fails. A `pending` result or transport error
must retry the same identity. Pending authorization survives cancellation/restart,
and revalidates its original snapshot before any new commitment. Native stores
only profile/network/request ID/token/revision/kind in a private local retry
journal, exposes a saved-confirmation resume action, and ignores late responses
from an earlier wallet/network generation. A definitive rejection clears the
journal and requires a fresh review. The bounded 1,000-receipt history fails
closed when full rather than forgetting an identity that might be retried.

Existing `CreateOffer`/`TakeOffer` remain compatible for explicit command-line
callers; they do not synthesize a native review or an idempotent confirmation
identity. New interactive clients should use the quote/confirm pair.

### Coin control and sends

Status includes `coins` (chain, outpoint, amount, address, confirmations, reserved)
and public `sends` (transaction ID, recipient, amount, total fee, change, submission
and confirmation state). Private signed transaction bytes are omitted.
`SendCoins` / CLI `wallet.send` requires a unique `id` (16–64 characters), `chain`,
`destination`, `amount`, `fee`, 1–50 `inputs` (`txid`, `vout`), and the displayed
`expected_network`. Amounts and the exact total miner fee are integer satoshis.
The daemon revalidates ownership, current unspent outputs and confirmations,
network/address, dust, and all order/trade/send reservations before signing. BTC
sends also require the same bounded chain-exclusive ancestry proof as BTC swap
funding. The fee range is 1–1,000,000 sats; the recipient must receive at least
600 sats, and change must be zero or at least 600 sats.

Open orders reserve full funding coins, including the funding fee. Cancelling an
unreserved order releases those coins; an accepted trade retains them until its
funding consumes them or it reaches a safe terminal state. A send is persisted
before broadcast and retries the same signed bytes after ambiguous failures.
Retry the same request ID with identical details to retrieve its existing result;
changing details with an existing ID is rejected. Transaction lookup errors do not
prove absence: the daemon retries broadcast only after an explicit not-found
result, at intervals of at least 30 seconds. `submitted` does not mean confirmed;
inspect `confirmations` and `error`. History is capped at 1,000 sends. Pending
sends block network switching until six confirmations. There is no send cancellation or fee replacement;
a low fee can leave a payment pending until miners accept it.

Unanswered take requests expire at the signed offer deadline before acceptance or
prepared funding. Their coins unlock, retries stop, and late acceptance cannot
revive the request. Already accepted trades retain their settlement deadlines.
An accepted maker reservation with no prepared funding expires when its signed
funding safety window closes, even if the taker never broadcasts. Its offer becomes
cancelled. Safe expiry is final across restart and clock rollback; signed funding
transactions retain their input locks and existing settlement obligations.

## Private protection and manual refresh

`CreateOffer.tower_bps/tower_pubkey` select protection for the maker only.
`TakeOffer.tower_bps/tower_pubkey` independently select the taker’s refund
protection. Zero disables that wallet’s protection; neither selection is relayed
to the counterparty. Order tower
fields are populated only for the authenticated local maker wallet.

`RefreshStatus` (`POST /v1/status/refresh`, CLI `status.refresh`) requires
`expected_network` in the desktop API and runs a fresh wallet processing cycle.
It returns `Status`; normal `GetStatus` reads the latest background snapshot.
A request during an existing cycle waits for a subsequent cycle so the chain reads
start after the refresh request. It can span two bounded 30-second cycles; the
native refresh call allows 70 seconds and shows failures without switching wallets.

### Available funds and replay preflight

`Status.funds` contains exact per-chain satoshi amounts. `total_confirmed` is
`unlocked_confirmed + reserved_confirmed`; `unconfirmed` includes every observed
deposit coin below the required depth (2 on regtest, 6 on public networks).
These deposit categories do not overlap. The legacy `balances` map remains total
confirmed funds. Whole reserved inputs are counted once, even if multiple durable
obligations reference one input. Pending change is never unlocked confirmed.

`htlc_locked` is the observed unspent principal in this wallet's own swap funding
outputs, separate from deposit coins. It is neither a spendable balance nor a
prediction of swap proceeds. Read it only when `htlc_available` is true; lookup
failure does not imply zero. Both mempool and confirmed unspent contracts are
included. Spent funding inputs are no longer included in the deposit partition.

`WalletCoin.holds` explains each reservation using an activity `kind` (`offer`,
`swap`, or `send`), its local `id`, a reason, and whether the order is cancellable.
Only `CancelOffer` can release an open order; signed funding and sends retain
reservations even across restart or reorg until their inputs are observed spent.
The native coin-control view links each hold to the owning activity and exposes
safe open-order cancellation.

`PreflightFunds`, HTTP `POST /v1/wallet/preflight`, CLI `wallet.preflight`, accepts
`chain`, `amount`, `fee`, `expected_network`, and optional explicit `inputs`.
Without inputs it evaluates the actual automatic funding candidate selection;
with inputs it evaluates that exact set. Results bind `network`, `wallet`, the
candidate `inputs`, `total`, and fee-inclusive `sufficient` status. Replay `state`
is `proven`, `not_proven` under the bounded verifier, `checking` when another check
is active, or `unavailable` for backend/observation failures. Blake reports
`not_applicable`. One BTC-exclusive ancestor in the selected set suffices; shared
inputs may accompany it. A missing indexer response never proves exclusivity.

Preflight does not sign, reserve, or authorize a later action. Its two-second
budget runs outside the settlement mutex with one concurrent check per wallet;
proofs are never cached. Repeat after a connection recovery or chain change.
Create/take/send still perform authoritative reservation/output checks, and BTC
funding and sends still run the conservative ancestry proof before publication.
Native forms discard checks after wallet, network, amount, fee, generation, or
selected-input changes. BTC coins that remain unproven require independently
split ancestry descended from a post-fork BTC coinbase; no splitting service is
provided.

## Fee review and acceleration

`QuoteFee` (`fee.quote`, `POST /v1/fees/quote`) returns per-chain native sat/kvB
estimates, freshness/source/targets, exact fee/change/principal, input selection
and a conservative vsize bound. Manual `fee` stays available without estimates.
Use the reviewed total in `SendCoins.fee` or create/take `funding_fee`, and carry
`rate_sat_kvb`/`fee_timestamp` when selecting an estimate. Funds preflight uses
that same fee. `max_fee` on a send and `owner_fee_cap=20000` on create/take are
explicit pre-funding authorizations; omitted caps preserve base-only behavior.

`BumpTransaction` (`transaction.bump`, `POST /v1/transactions/bump`) requires
activity ID, kind, higher total fee, expected current transaction ID and network.
Send status retains all variants and a separate state; swap status exposes
current owner settlement fees/IDs and authorized variants. Funding acceleration
is refused. See [the complete fee contract and recovery limits](FEES.md).

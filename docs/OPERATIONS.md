# Configuration, operation, and recovery

## Desktop network settings

The app starts its own wallet daemon and defaults to mainnet. Settings contains
separate regtest, Testnet4, and mainnet node/relay profiles. Choose the active
network separately from the environment being edited. Save applies configuration
to the daemon and reconnects; switching networks is blocked until active local
offers/swaps have finished. A broken current endpoint does not bypass that guard.
Use connection edits within that environment to recover connectivity.

| Environment / chain | Default endpoint | Verification / requirement |
| --- | --- | --- |
| Mainnet BTC | `ssl://electrum.blockstream.info:50002` | Public Electrum, system CA verification |
| Mainnet Blake2b | `ssl://fulcrum.kilombino.com:17717` | Public fork-compatible Fulcrum, explicit certificate pin |
| Testnet4 BTC | `ssl://mempool.space:40002` | Public Electrum, system CA verification |
| Testnet4 Blake2b | Empty | Configure a fork-compatible own node/indexer; no verified public default found |
| Regtest BTC | `http://127.0.0.1:19443` | Separately operated Core RPC with cookie file |
| Regtest Blake2b | `http://127.0.0.1:29443` | Separately operated activated Blake2b RPC with cookie file |

Public endpoints were read-checked on September 5, 2026: genesis, fork identity,
header formats/proof of work, height, coinbase raw transaction and merkle inclusion.
No real-fund transactions were broadcast as part of those checks. The Blake2b
operator [publishes the Fulcrum host and port](https://kilombino.com/). Bitcoin
endpoints are listed in upstream [Electrum server configuration](https://github.com/spesmilo/electrum/tree/master/electrum)
and [Sparrow server configuration](https://github.com/sparrowwallet/sparrow/tree/master/src/main/java/com/sparrowwallet/sparrow/net).

The shipped Blake2b server's self-signed leaf certificate was observed with DER
SHA256 fingerprint:

```
506dadc710c5abaeb13191056c5aaf47035d30e08bd869f7b4fbe6e13745d5a7
```

An explicit pin replaces CA validation for that server but still checks validity
dates. This fingerprint was obtained from a live connection, not an independently
signed operator attestation. Verify a replacement through a trusted operator
channel before changing it. Leave the pin empty to use normal system CA validation
for your own TLS server. Plain `tcp://` Electrum is accepted only on literal
loopback. Electrs/Fulcrum must support the actual chain's headers and history;
pointing the Blake2b setting at ordinary Bitcoin Electrum is rejected.

Default Nostr relays are `wss://nos.lol`, `wss://relay.primal.net`, and `wss://relay.ditto.pub`. They are public
services used by the [nak](https://github.com/fiatjaf/nak), [Primal](https://github.com/PrimalHQ), and [Ditto](https://gitlab.com/soapbox-pub/ditto) ecosystems. Read-only checks of the actual orderbook and mailbox subscriptions passed for all three, including NIP-42 authentication for Ditto's mailbox reads; this does not guarantee future write admission.
Configure one to three shared relays per environment; public connections require
WSS and local test relays may use loopback WS. Public relay retention/admission
is not guaranteed by availability checks. Relay settings do not grant a relay
custody, chain validation authority, or orderbook consensus.

Settings saved by an early development build may still contain the unavailable
`wss://dmrelay.com`. Replace that entry with `wss://relay.ditto.pub` in each affected
environment and save. Defaults do not overwrite previously saved custom settings.
Relay errors include the failing endpoint. An `auth-required:` read rejection is
retried after the daemon authenticates its Nostr identity; a relay that still
rejects the authenticated identity needs different admission permissions or must
be replaced in Settings. The daemon continues checking the other configured relays.

For full-node RPC, choose `rpc`, enter an explicit loopback HTTP or remote HTTPS
URL, and an absolute local cookie file path (`username:password`). Credentials are
read locally and sent only as HTTP Basic auth to that configured endpoint; never
embed them in the URL. The node must expose its wallet API and transaction index,
with the selected network and Blake2b activation. Mainnet activation is 961640;
Testnet4 is 150308; the fixture activates regtest at 1. The default regtest cookie
settings leave the cookie fields empty to use the explicit local launcher
registration described under [Local regtest discovery](#local-regtest-discovery).
Custom nodes can use explicit cookie paths. The app does not start or own nodes.

Regtest requires both external nodes to be running. In Settings or onboarding,
use **Choose…** beside each RPC cookie path to select the hidden `.cookie` file
from that node's `regtest` data folder. Explicit paths must match your node’s
actual datadir; leave the field empty only when using registered local fixtures. For nodes started with `scripts/local.py nodes`,
use the checkout's `.local/btc/regtest/.cookie` and
`.local/blake/regtest/.cookie`, with the script's configured RPC ports. Test both
connections and save Settings. Selecting a trading network also shows that
network's connection settings. BTC and BLAKE each show a loading indicator until
their own connection and wallet history are ready, then display the observed height.

The **Check connection** action reads chain identity/height and displays the
observation trust model. It does not fund an address or post a Nostr offer. Every wallet also serves watchtower jobs while open, at a default 50 basis-point (0.50%)
rescue fee. Settings → Watchtowers configures that fee per network from 0.01% to
10.00% in 0.01% steps, applying to every local wallet on that network. Save to
refresh signed public and private quotes. Accepted rescue jobs and retries keep
their agreed fee; new registrations must match the current quote. Legacy settings
without `rescue_fee_bps` (or with zero) retain the 0.50% default. The own rescue fee
is independent of a selected external provider's quote.
Public listing is **off by default**. Settings exposes a network-specific
npub with Copy and a **Show my watchtower in the public list** toggle. Save settings
to apply it. Opting out publishes a signed withdrawal when previously public;
relays may retain historical announcements.

Add a provider from the public list or use **Look up & add** with a shared npub.
Private lookups and replies use encrypted Nostr mailboxes, so unlisted providers
remain usable. Save favorites, then select one when enabling delayed protection
in Create offer. The provider generates and signs both payout scripts and its fee;
no script hex entry is needed. Each wallet independently selects and privately
pins its own provider quote; the orderbook shows protection only to the maker,
and Take offer offers a separate taker refund choice. Neither choice is included
in the public offer or shared terms. Protected funding still requires durable
receipts. Discovery and private lookup require a shared relay and a reachable provider; announcements expire after one hour and
refresh every fifteen minutes. Listing does not prove reliability.

## Explicit developer fixtures

Build outputs and `.cache`/`.local` data are ignored by Git. The normal app data
layout and lifecycle are documented in [Packaging](PACKAGING.md). The following
commands create independent local services, separate from the app bundle:

Both nodes have P2P listening, automatic connections, DNS seeds, discovery, and NAT mapping disabled. The Blake2b node receives `-testactivationheight=blake2b@1`; ordinary regtest without this flag would not test the fork. Nodes use `txindex=1` so relevant historical transaction data remains available to the scanner.

The full node's watch-only wallet is named from the trader's public Nostr identity. RPC imports address-only descriptors with private keys disabled. The separate `faucet` wallet is solely a source of valueless test coins.

The node archives are fetched over HTTPS and checked against the upstream SHA256SUMS. This detects corruption; the current bootstrap does not independently authenticate release signatures. Do not reinterpret this local-development bootstrap as a production binary-verification process.

## Lifecycle commands

```sh
python3 scripts/bootstrap.py             # pinned node downloads, cached
python3 scripts/local.py nodes            # start and initialize both nodes
python3 scripts/dev.py up                 # build/start all services and seed demo balances
sh scripts/build-mac.sh                   # build the macOS app

python3 scripts/dev.py status
python3 scripts/dev.py down               # stop app processes, preserve data/nodes
python3 scripts/local.py stop-nodes       # stop both full nodes
```

`dev.py up` preserves existing vaults. It tops up demo wallet balances when below one test coin; this is a convenience of the local launcher, not an exchange deposit service. If a process is already running, rebuilds do not replace its executable in memory: stop and start app processes to run a new daemon build. Do that between trades or within the remaining safety margins.

To drive a single daemon directly:

```sh
bin/blakeswap daemon --config .local/alice/config.json
bin/blakeswap call --socket "$PWD/.local/alice/daemon.sock" --method status
```

Config contains `network` (empty preserves legacy regtest), `name`, `mode` (`trader` or `tower`), absolute data/password/socket paths, one to three relay URLs, both nodes (`kind`: `rpc` or `electrum`, `url`, RPC `cookie`, optional `certificate_sha256`), and selected tower public identity, payout scripts, and basis-point quote. All modes serve watchtower jobs; `tower` disables trading and may set its serving rate through `tower.bps`. `public_watchtower` opts into listing (default false), and `favorite_watchtowers` stores npubs for private quote refresh. Status returns `own_watchtower` and authenticated discovered `watchtowers`. Legacy configured trader quotes can still be selected explicitly by local CLI commands. Each party selects protection independently; the maker’s choice is no longer imposed on or disclosed to the taker. Do not change tower identity or scripts mid-swap: accepted terms and signed templates intentionally retain their original commitment.

## Local API

See [API](API.md) for the protobuf schema, gRPC connection/authentication, HTTP
bindings, OpenAPI generation, and CLI method mapping. The old newline-JSON Unix
socket protocol has been removed. For the app, read endpoint discovery from its
private runtime file; standalone daemons retain the configured socket path.

## Backups

Use **Wallet → Save encrypted state backup** or `wallet.backup`. The snapshot is encrypted with the profile's current vault password; it includes the seed and pending-swap data. Store the matching password separately and securely. The mnemonic alone cannot reconstruct a random swap preimage or every previously issued rescue signature.

For recovery of this local test environment, stop the affected daemon, preserve its current database, and restore the matching backup to `state.db` with mode 0600. Keep the matching `vault.password` and same chain data/configuration. Start the daemon and inspect both-chain observations and every pending swap before relying on it. Do not copy a vault while a writer is active; use the consistent backup command.

Restoring stale snapshots is not generally safe automatic recovery. Previously shared signatures and preimages remain valid even if the snapshot predates them. See the explicit [rollback limitation](RISKS.md).

## Troubleshooting

**Daemon disconnected:** the desktop reconnects automatically; inspect the app data directory's `desktop.log`; for the separate CLI fixture, use `python3 scripts/dev.py up` and inspect `.local/<name>.log`. Check Settings and the configured endpoint before changing networks. A locked desktop does not stop the daemon; a sleeping/offline machine can stop timely responses.

**Insufficient balance:** on regtest RPC, use the test faucet from Wallet, then mine two blocks. On public networks, wait for confirmations and ensure BTC inputs meet replay-ancestry requirements. Total confirmed funds include available and reserved deposit coins. The available amount excludes whole coins held by local orders, trades, or sends; awaiting-confirmation change is shown separately. Own unspent contract principal is a separate observation, not available funds. Coin control links each hold to its order, swap, or send; the activity view can cancel an unreserved open order to release its coins. Local reservations prevent honest-client reuse, but remote offers are not proof of funds. Funding still verifies actual unspent coins.

**Awaiting durable tower receipt:** ensure the selected tower and at least one shared relay are running. The daemon will not fund a protected leg based solely on a relay acknowledgment. Expired headroom may require a fresh offer/swap rather than continuing stale terms.

**Awaiting chain confirmations:** wait for public network blocks; mine only on explicit regtest fixtures. Avoid advancing the chain arbitrarily while participants are negotiating, because locktimes do not pause.

**Secret reveal cutoff reached:** the honest daemon refuses first revelation. It continues watching and allows its own refund when eligible. The counterparty may also refund, and configured towers become eligible after their refund grace.

**Contested outcome:** inspect both spend IDs and chain histories. A claim/refund split reflects violated liveness/security assumptions, not a state that the relay or a UI cancellation can reverse.

**Relay history/storage limit:** the local implementation fails visibly at bounded capacities. Archive/rebuild a disposable regtest environment only after completing or safely recovering all swaps. There is no production retention/compaction workflow.

### External RPC wallet synchronization

An external full node needs an unpruned, indexed chain and descriptor-wallet RPC
support, including `gettxspendingprevout` (the pinned Core/Knots 29 nodes support
it). The daemon imports only public deposit-address descriptors. Initial imports
scan historical blocks from timestamp zero, preserving deposits when restoring a
wallet or moving from Electrum to RPC. This can take hours on mainnet. Desktop
bootstrap runs independently of the short trading cycles; status and Settings
remain available. History imports run independently with retained completion
results; cancellation of a short trading cycle does not discard a completed
rescan. Each chain publishes its own readiness after its wallet history
synchronizes; funding and first revelation require both. Existing wallets keep
running while a new wallet initializes. Changing the active
network or its connections, or quitting, cancels bootstrap and releases wallet locks.

After a complete successful import response, the daemon records the
`blakeswap-history-ready-v1` address label in that node's watch-only wallet. On
reconnect it checks both the descriptor's historical timestamp and this label,
avoiding a new historical scan. A descriptor without the completion label is not
considered ready. If the app loses the response or exits during a scan, the node
may finish independently; the next connection waits while the node reports a
scan and then conservatively repeats that unacknowledged import once. Pruned or
failed historical scans remain errors. Do not manually set the readiness label
on an incompletely scanned wallet.

Mempool spend observation queries the watched outpoints with
`gettxspendingprevout` and downloads only relevant spending transactions. It does
not enumerate or download the public mempool. Confirmed block scanning and reorg
checks remain separate.

## Local regtest discovery

From a source checkout, run `make regtest-nodes` for both chains, or
`make regtest-btc` / `make regtest-blake` for one. These targets download the pinned,
checksum-verified upstream nodes, start isolated regtest data under this checkout's
`.local/`, prepare the faucet wallet, and register the actual endpoint/cookie paths.
`make regtest-stop` stops this checkout's nodes. No public-network node is started.

An empty RPC cookie field opts local regtest connections into automatic discovery.
Fresh settings use it by default; obsolete generated paths are migrated when those
files no longer exist. Explicit custom paths stay authoritative. Registration lives
in `~/Library/Caches/Blakeswap/regtest-nodes.json` on macOS, outside wallet storage,
so resetting onboarding or creating another wallet does not lose the node location.
It records paths, never cookie contents. Restarting the launcher from a different
checkout updates its registration. Non-listening endpoints produce a node-unreachable
error with the Make command; reachable nodes without registration request a cookie.
External/custom nodes can still be configured by endpoint and cookie file.

Default ports are 19443 (BTC) and 29443 (Blake2b). `BLAKESWAP_BTC_RPC_PORT` and
`BLAKESWAP_BLAKE_RPC_PORT` override launcher ports; set the matching endpoints in
Settings when using custom ports. `BLAKESWAP_REGTEST_REGISTRY` overrides the registry
path in both launcher and helper for isolated testing. Ordinary `scripts/local.py
nodes` does not register anything unless `--register` is supplied.

## Receiving and recovery

The Wallet view can display a QR containing the exact current address, labeled
with its chain and network. A confirmed receipt rotates only that chain's receive
address; mempool transactions do not rotate it. Spendable balances retain the
existing network confirmation policy. Earlier addresses continue receiving funds
and remain included in balance and funding selection.

Encrypted state backups preserve all allocated receive indexes. Phrase or older
backup recovery scans confirmed receipt history, including spent outputs, from
the saved index until it finds an unused address. Preserve current state backups:
a phrase alone cannot reconstruct an address-use record erased by a reorg, nor
recover pending swap state. Existing signed swap/tower transactions retain their
original payout destinations. Change and newly prepared swap payouts use the
current receive address; watchtower quotes use stable index-zero payout scripts.
Those shared destinations and combined-input transactions retain privacy costs
despite rotation. The current receive-index limit is 10,000 per chain.


## Sending and coin control

Choose **Send BTC** or **Send BLAKE** in Wallet. Enter the recipient, amount in sats,
and exact total network fee in sats; select 1–50 individual confirmed coins and
choose **Review send**, then **Confirm and send** after checking the chain, network,
destination, amount, fee and change. **Send selected minus fee** fills the amount
with the selected total less the fee, leaving no change. Sending this to your own
current receive address consolidates the selected coins; there is no automatic
consolidation service. The review checks the selected inputs and fee; BTC sends also check replay-safe ancestry. The send screen
uses the displayed wallet and network throughout. The daemon checks them again.
Locked coins are visible but cannot be selected. Open **View activity** on a hold
to inspect its owning order, swap, or send and safely cancel an open order to free its
coins; a reserved order has become a trade and cannot be cancelled to withdraw its
funding. Pending sends also retain their inputs. The daemon persists the exact
signed transaction before broadcast; ambiguous failures retry those same bytes.
Retry an uncertain API request with the same ID and identical details rather than
creating another payment. **Outgoing sends** shows transaction IDs, amounts, fees,
confirmation counts, and errors in Wallet; submission alone is not confirmation.
Select a chain-specific estimate or a manual total, then review the amount, change,
and replacement maximum. **Increase fee** uses change within that original cap;
all signed variants remain tracked. A rejected or ambiguous broadcast remains
saved for retry. See [Fees and recovery](FEES.md) for stuck states, owner/tower
escalation, and the explicit refusal of funding replacements. Send cancellation
is unsupported. Sends below six
confirmations block network switching, including on regtest; send history is
currently capped at 1,000 records.

The maker serializes take requests and durably reserves an available order for one
trade before sending acceptance. Other takers receive a rejection; a local pending
request also hides that offer from Take. This follows Bisq's maker-side available →
reserved transition and persistence pattern in
[TradeManager](https://github.com/bisq-network/bisq/blob/master/core/src/main/java/bisq/core/trade/TradeManager.java)
and [OpenOfferManager](https://github.com/bisq-network/bisq/blob/master/core/src/main/java/bisq/core/offer/OpenOfferManager.java).
Relay propagation is asynchronous: another user can briefly see a stale offer,
but cannot obtain a second accepted reservation from this daemon. Remote makers
can run different software and advertise unbacked offers. Unanswered take requests
expire at the signed offer deadline only before acceptance or prepared funding;
accepted maker reservations with no prepared funding can expire when their
funding window closes. Those safe expiry paths release unsigned intentions, not
funded swaps. Never delete local state to try to cancel signed or published funding.

After initial recovery, live RPC address allocations import from the current time
and carry a separate completed-import marker, avoiding a historical rescan during
trading. An unfamiliar/restored address still scans history. Wallet polling checks
the current address plus up to eight historical addresses per cycle, rotating
through all old addresses. A late payment's balance can therefore take multiple
cycles to appear in a large wallet. Selected inputs are checked directly on chain
again before a funding transaction or send is signed.


### BTC readiness and available funds

Create, take, and send forms check fee-inclusive unlocked confirmed candidates.
BTC readiness is **proven**, **not proven** by the bounded ancestry verifier,
**checking**, or **unavailable**. A set containing one proven exclusive input can
include shared-history inputs; each input need not be independently exclusive.
If a chain or indexer is unavailable, restore both connections and choose **Check
funds again**. A not-proven result may mean shared ancestry or bounded-proof
exhaustion; use independently split BTC with a post-fork BTC coinbase ancestor,
or select a suitable mixed set. There is no automatic splitting service.

Checks use a two-second budget outside the settlement mutex and keep no proof
cache. Wallet/network/form/input changes invalidate native readiness. A successful
check does not reserve funds or guarantee future availability; the daemon keeps
its authoritative output, reservation, and ancestry checks before funding.


## Endpoint failover and partial connectivity

Settings keeps the existing primary server and accepts up to three ordered
fallbacks for each chain/network. Existing single-server settings remain valid
as a one-entry list; no wallet seed, database, signed transaction, negotiated
terms, or reservation is replaced by this compatibility migration. Each entry
has its own backend, URL, RPC cookie path, and optional Electrum certificate pin.
Use **Test connection** / **Test fallback** for each candidate, then save Settings.
An active healthy secondary remains selected; restart/reconfiguration begins at
the primary again. Backoff is 2–32 seconds after errors. Transport attempts are
bounded to two seconds; each chain has eight seconds of cumulative backend
work per cycle, and wallet refresh/local scan phases each have a five-second
limit. Relay time and the other chain do not consume that chain’s allowance. Long RPC history imports continue independently,
with shutdown cancelling and joining them before closing transports.

A candidate must pass the configured network/genesis/fork rules, transport
security, and its own configured certificate pin before use. Pins require TLS;
RPC pins are refused rather than ignored (RPC HTTPS uses normal CA validation).
A new source must reach and agree with the previous source's last observed tip.
A lagging candidate reports **stale candidate tip**; differing history reports
**conflicting chain history**. A legitimate reorg reported by the recovered
previous source can update that anchor, allowing an agreeing fallback afterward.
If the old source cannot recover, independently verify the correct history and
reconfigure the chain endpoints deliberately. Do not blindly change a pin or
server to clear an error. This routing is not most-work validation or a quorum.

Header caches, watch-only history imports and scan cursors belong to individual
endpoints. Switching sources invalidates the combined wallet observation and
requires a complete refresh before trading. Settings shows the active source,
each endpoint's last error/retry time, and failover count. Public status retains
last observed heights/balances with `connections[chain].ready=false`; the native
balance card labels those values as old observations. Unavailable data is never
reported as a fresh zero balance.

| Action with one chain unavailable | Required evidence / behavior |
| --- | --- |
| Wallet monitoring and retries of an already saved signed send | Fresh target-chain observation; retry only the same signed bytes and keep reservations after ambiguous errors. New sends still require their normal full refresh/replay checks. |
| Swap spend monitoring | A successful scan from the available chain; unavailable-chain observations remain explicitly stale. A witnessed preimage is persisted monotonically. |
| Owner claim | A preimage already observed in a validated contract spend, including after restart; fresh target scan and exact confirmed unspent agreed HTLC. A private generated secret or a claim signed before a crash is insufficient. |
| Tower claim | Previously witnessed/persisted secret, fresh target-chain scan, existing authorized signed templates and target locktime checks. |
| New funding, retries of swap funding, first revelation, owner/tower refunds | Held until the required observations of both chains return. Missing peer observations never authorize a refund. |

Once an incoming claim has been observed, the owner permanently suppresses its
refund path even if that witness is later reorged away. Recovery still depends on
timely valid observations and confirmation; endpoint failover does not remove
chain censorship, finality, pinning, or malicious-indexer risks.

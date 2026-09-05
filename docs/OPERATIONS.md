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
paths are under your standard Bitcoin/BitcoinBlake2b application-support
folders; update them for your own datadirs. No automatic node discovery or node
process ownership is implied.

The **Check connection** action reads chain identity/height and displays the
observation trust model. It does not fund an address or post a Nostr offer. Every wallet also serves watchtower jobs while open, at a 50 basis-point (0.50%)
rescue fee. Public listing is **off by default**. Settings exposes a network-specific
npub with Copy and a **Show my watchtower in the public list** toggle. Save settings
to apply it. Opting out publishes a signed withdrawal when previously public;
relays may retain historical announcements.

Add a provider from the public list or use **Look up & add** with a shared npub.
Private lookups and replies use encrypted Nostr mailboxes, so unlisted providers
remain usable. Save favorites, then select one when enabling delayed protection
in Create offer. The provider generates and signs both payout scripts and its fee;
no script hex entry is needed. Both parties use the selected quote pinned in the
signed offer. A taker can inspect the provider npub in the orderbook. Protected
funding still requires durable receipts. Discovery and private lookup require a
shared relay and a reachable provider; announcements expire after one hour and
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

Config contains `network` (empty preserves legacy regtest), `name`, `mode` (`trader` or `tower`), absolute data/password/socket paths, one to three relay URLs, both nodes (`kind`: `rpc` or `electrum`, `url`, RPC `cookie`, optional `certificate_sha256`), and selected tower public identity, payout scripts, and basis-point quote. All modes serve watchtower jobs; `tower` disables trading and may set its serving rate through `tower.bps`. `public_watchtower` opts into listing (default false), and `favorite_watchtowers` stores npubs for private quote refresh. Status returns `own_watchtower` and authenticated discovered `watchtowers`. Legacy configured trader quotes remain supported for older CLI offers. Do not change tower identity or scripts mid-swap: accepted terms and signed templates intentionally retain their original commitment.

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

**Insufficient balance:** on regtest RPC, use the test faucet from Wallet, then mine two blocks. On public networks, wait for confirmations and ensure BTC inputs meet replay-ancestry requirements. Confirmed balance excludes unconfirmed change and locked HTLCs. Multiple open offers can overstate available inventory; reservation is serialized and funding still verifies actual unspent coins.

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
remain available. Each wallet becomes ready after both chain histories synchronize;
existing wallets keep running while a new wallet initializes. Changing the active
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

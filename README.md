# Blakeswap

A native macOS client and Go daemon for asynchronous, noncustodial Bitcoin ↔ Bitcoin Blake2b atomic swaps. Signed offers and encrypted swap messages travel through Nostr relays. Optional watchtowers can execute delayed, pre-signed rescues and earn a percentage only when their rescue transaction confirms.

The app supports mainnet, Testnet4, and regtest. Mainnet starts with public Electrum
servers; Settings accepts your own Electrum or full-node RPC endpoints per chain
and environment. The native client connects directly to the Go daemon using protobuf/gRPC; an
authenticated grpc-gateway HTTP API and OpenAPI description are also provided.
There is no token, DAO, treasury, custody server, or upfront watchtower fee.

The protocol is experimental and has not received an independent security audit.
Electrum operators are a trust dependency for chain observations. BTC-side funding
must prove conservative chain-exclusive ancestry, and public-network timing/fees
have explicit limitations. Read [Risks](docs/RISKS.md) before considering real funds.

## Build and install

Repository and Go module: `github.com/blakeswap/blakeswap`.
Requires macOS 15+, compatible Xcode/Swift 6.1+ tooling, Go toolchain downloads, and Python 3.

```sh
git clone https://github.com/blakeswap/blakeswap.git
cd blakeswap
sh scripts/build-dmg.sh
```

The [release workflow](.github/workflows/release.yml) defines separate Apple silicon
(`arm64`) and Intel (`x86_64`) DMGs. Check [GitHub Releases](https://github.com/blakeswap/blakeswap/releases)
for available artifacts, or build locally; the workflow alone is not evidence that
a particular release has built or shipped.
Open `bin/Blakeswap-0.2.0-arm64.dmg` (or the Intel equivalent), drag the app into Applications, and open it.
On first launch, choose a wallet name and create a new wallet or restore a BIP39
recovery phrase or encrypted state backup. Setup checks three recovery words,
offers a password-protected backup file, and walks through network and server
connections. Existing installations keep their wallets and skip onboarding.
The app owns one bundled Go daemon: launch starts it, quit stops it, and parent-death
monitoring stops it after a force quit.
The default build is ad-hoc signed; [Packaging](docs/PACKAGING.md) describes Developer
ID signing, notarization, the data layout, and a separate local regtest demonstration.

Settings selects the active network, node endpoints, Nostr relays, and watchtower favorites. Every wallet serves watchtower jobs while the app is
open; public listing is off by default and can be enabled in Settings. Copy your
npub to share privately, or discover public providers and save favorites. Payout
scripts and fee quotes are generated and signed by each provider. Public BTC and Blake2b mainnet defaults have been checked against live
chain data. No verified public Blake2b Testnet4 indexer was found; configure your
own endpoint for that chain. The application does not substitute a Bitcoin server
or silently change networks. [Operations](docs/OPERATIONS.md) lists the defaults and
trust assumptions.

Wallet provides a QR for each chain’s current receive address and advances it after
a confirmed receipt while continuing to watch historical addresses. Send supports
individual coin selection, chain-specific fee estimates or manual totals, and review/confirmation of
the payment. “Send selected minus fee” can consolidate selected coins to your own
current receive address. Open orders, active trades, and pending sends lock their
funding coins; cancel an open order before withdrawing those coins. Saved sends
retry the same signed transaction and show outgoing IDs, confirmations, and errors
in Wallet. Balance cards distinguish total, available, reserved, and awaiting
confirmation funds, with observed contract principal shown separately. Forms check
fee-inclusive available inputs and BTC replay ancestry; coin-control activity links
explain holds and permit safe open-order cancellation. See [sending and coin control](docs/OPERATIONS.md#sending-and-coin-control).

Create offer and Take offer first show a daemon-backed economics review: your
outgoing principal and funding fee, incoming net range, conditional tower costs,
and expected timing/reveal duties. BTC and BLAKE costs stay separate. Confirming
once authorizes the automatic swap sequence within the reviewed limits. Cancelling
the review commits nothing. If the response is lost, use the saved-confirmation
resume action to retry the same request after restart. See [reviewed swaps](docs/API.md#reviewed-swaps).

## Local regtest nodes

Run `make regtest-nodes` for both chains, or `make regtest-btc` / `make regtest-blake`
for one chain. Then select Regtest in the app with the cookie fields empty; the app
discovers the launcher's current cookie paths automatically. `make regtest-stop`
stops the nodes. See [Operations](docs/OPERATIONS.md#local-regtest-discovery).

## Desktop regtest demonstration

```sh
sh scripts/build-mac.sh
python3 scripts/desktop-demo.py prepare
open bin/Blakeswap.app --args --data-dir "$PWD/.local/desktop-demo"
python3 scripts/desktop-demo.py trade
```

This developer harness starts regtest nodes and a relay, and
configures an isolated app data directory to connect to them. It does not change
the normal desktop wallet or add those services to the app bundle. The trade
exchanges 1,000,000 BTC sats for 2,000,000 BLAKE sats using the app-owned daemon.
Alternatively use Alice's Create offer, Bob's Take offer, and the regtest mining
controls in the native UI. “BLAKE” is this app's label for Bitcoin Blake2b.

Closing the app stops its daemon; chain deadlines continue. A maker must return to
accept and fund a new swap, but both participants need not be online simultaneously.
An armed external watchtower can claim/refund only its pre-authorized jobs. It
cannot negotiate or perform a taker's first secret revelation.

## Command-line demonstration

```sh
python3 scripts/dev.py up
python3 scripts/dev.py trade
python3 scripts/dev.py status
python3 scripts/dev.py call alice wallet.backup
python3 scripts/dev.py down
python3 scripts/local.py stop-nodes
```

This separate CLI harness runs independent daemons. `trade` uses the same typed gRPC API as the GUI and writes a transaction-level result to `.local/successful-trade.json`. It mines only when transactions are waiting. `down` stops application processes; the separate `stop-nodes` command stops the full nodes. Startup is idempotent and preserves data.

Wallet addresses, daemon identities, heights, signed offers, transaction IDs, and current settlement status are available from `status`. Mnemonics and private preimages are omitted. Recovery phrases require the explicit `wallet.recovery` command or the corresponding Wallet screen button.

## Tests

```sh
sh scripts/test.sh
```

The harness runs unit tests, upstream NIP-44 vectors, race detection, bounded fuzz campaigns, and actual transactions on both regtest chains. It exercises asynchronous participation with daemon shutdown/reopen at handoffs, tower takeover, both-party refunds, reverse-direction swaps, owner fee avoidance, transaction tampering, durable mailboxes, and reorg recovery. It adds test blocks and spends test coins on the local nodes; finish any manually timed demonstration first.

See [the invariant matrix and test limits](docs/TESTING.md). “Comprehensive” does not mean a finite suite proves every cryptographic or distributed-system schedule.

## Documentation

- [Architecture, components, and trust boundaries](docs/ARCHITECTURE.md)
- [Exact protocol, transactions, messages, and state transitions](docs/PROTOCOL.md)
- [Watchtower economics and fee calculation](docs/ECONOMICS.md)
- [Limitations, risks, and recovery assumptions](docs/RISKS.md)
- [Network settings, endpoints, operation, and backups](docs/OPERATIONS.md)
- [Protobuf, gRPC, HTTP gateway, and OpenAPI](docs/API.md)
- [Native app lifecycle, DMG, signing, and installation](docs/PACKAGING.md)
- [Test coverage and reproducible verification](docs/TESTING.md)
- [Completed local demonstration and transaction evidence](docs/VERIFICATION.md)

Source lives in `internal/` and `cmd/blakeswap/`; native SwiftUI source lives in `macos/Blakeswap/`. Build products, downloaded binaries, regtest data, credentials, and wallet state remain in ignored directories.

Wallets can be created and renamed in Settings. New installations set up their
first wallet during onboarding; existing Alice/Bob vaults are preserved. The selector lists saved wallets
on every network, and all wallets continue trading and serving watchtower jobs
while the app is open. Names are labels: renaming does not change keys, addresses,
or balances. Public watchtower listing is off by default; its per-network toggle
opts all of the app's wallets into that network's public directory.

To exercise first launch again, quit the app and run `make reset-local-data`.
This archives `~/Library/Application Support/Blakeswap` beside its original
location and prints the archive path. The next launch starts onboarding.
Use `make reset-local-data APP_DATA_DIR="/absolute/test/data"` for an isolated
installation. The archive contains the previous wallets and pending swap state;
keep it if you need to resume them. See [setup and recovery](docs/PACKAGING.md#first-launch-and-reset).

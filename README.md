# Blakeswap

A native macOS client and Go daemon for asynchronous, noncustodial Bitcoin ↔ Bitcoin Blake2b atomic swaps. Signed offers and encrypted swap messages travel through Nostr relays. Optional watchtowers can execute delayed, pre-signed rescues and earn a percentage only when their rescue transaction confirms.

**This implementation is deliberately restricted to local regtest.** It uses actual Bitcoin Core and Bitcoin Blake2b/Knots binaries, including the Blake2b fork's active consensus rules and unified signatures. It cannot connect the wallet to mainnet. There is no token, DAO, treasury, custody server, or upfront watchtower fee.

## Run locally

Repository: [blakeswap/blakeswap](https://github.com/blakeswap/blakeswap). The Go module and internal import prefix are `github.com/blakeswap/blakeswap`.

Requirements: Apple Silicon Mac, Xcode with command line tools, Go 1.24+ with automatic toolchain downloads, and Python 3.10.12+. The scripts pin Go 1.26.8, Bitcoin Core 29.1, Bitcoin Knots 29.4.1.knots20260508, and Go module versions. Initial downloads need internet access; the running demonstration uses loopback only.

```sh
git clone https://github.com/blakeswap/blakeswap.git
cd blakeswap
python3 scripts/bootstrap.py
python3 scripts/dev.py up
sh scripts/build-mac.sh
open bin/Blakeswap.app
```

The startup command builds the daemon, starts both regtest nodes, activates Blake2b at height 1, mines mature faucet funds, starts two persistent Nostr relays, and starts independent Alice, Bob, and tower daemons. Each trader receives test coins on both chains. Existing wallets and pending swaps survive restarts.

In the app:

1. Select **Alice · maker**, choose **Create offer**, and publish the default offer.
2. Switch to **Bob · taker** and choose **Take offer**.
3. Wait for the first funding transaction, then click **Mine 2 blocks on both chains**.
4. Wait for the maker's funding transaction, then mine two more blocks.
5. Once both claims appear, mine two more blocks. Both legs show **Completed**.

The default example exchanges 0.01 BTC for 0.02 BLAKE. “BLAKE” is this application's display label for Bitcoin Blake2b. The demonstration tower quotes 50 basis points (0.50%); it earns nothing when the owner claims first. These are configurable example terms, not a recommendation or a protocol-mandated market rate.

Closing the GUI leaves daemons running. **Pause** stops a daemon's trading, mailbox processing, and rescue actions; it does not pause blockchain deadlines. A stopped maker can later accept an unfunded request. After funding, either the wallet daemon or its armed tower must act within the available margins.

## Command-line demonstration

```sh
python3 scripts/dev.py trade
python3 scripts/dev.py status
python3 scripts/dev.py call alice wallet.backup
python3 scripts/dev.py down
python3 scripts/local.py stop-nodes
```

`trade` uses the same daemon API as the GUI and writes a transaction-level result to `.local/successful-trade.json`. It mines only when transactions are waiting. `down` stops application processes; the separate `stop-nodes` command stops the full nodes. Startup is idempotent and preserves data.

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
- [Local operation, configuration, API, and backups](docs/OPERATIONS.md)
- [Test coverage and reproducible verification](docs/TESTING.md)
- [Completed local demonstration and transaction evidence](docs/VERIFICATION.md)

Source lives in `internal/` and `cmd/blakeswap/`; native SwiftUI source lives in `macos/Blakeswap/`. Build products, downloaded binaries, regtest data, credentials, and wallet state remain in ignored directories.

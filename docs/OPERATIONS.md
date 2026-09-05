# Local operation and recovery

## Files and processes

| Location | Contents |
| --- | --- |
| `.cache/nodes/btc/` | Pinned Bitcoin Core binary/archive/checksums |
| `.cache/nodes/blake/` | Pinned actual Blake2b/Knots binary/archive/checksums |
| `.cache/go-*`, `.cache/swift/` | Local toolchain and compilation caches |
| `.local/btc/`, `.local/blake/` | Independent regtest nodes, RPC cookies, blocks, faucet wallets |
| `.local/alice/`, `.local/bob/`, `.local/tower/` | Config, encrypted state, vault password, Unix socket |
| `.local/relay-a.db`, `.local/relay-b.db` | Persistent public offers and encrypted gift wraps |
| `.local/*.log`, `.local/*.pid` | Application process logs and managed process identifiers |
| `bin/blakeswap` | Go daemon/relay/API client executable |
| `bin/Blakeswap.app` | Native SwiftUI macOS application |

These locations are ignored by Git. Never commit `.local`, wallet passwords, mnemonics, state backups, or raw private preimages. Protocol documentation and public transaction IDs do not require those secrets.

## Network configuration

| Service | Endpoint |
| --- | --- |
| Bitcoin Core RPC | `http://127.0.0.1:19443` |
| Bitcoin Blake2b RPC | `http://127.0.0.1:29443` |
| Relay A | `ws://127.0.0.1:7447` |
| Relay B | `ws://127.0.0.1:7448` |
| Each trader/tower API | `.local/<profile>/daemon.sock`, mode 0600 |

Both nodes have P2P listening, automatic connections, DNS seeds, discovery, and NAT mapping disabled. The Blake2b node receives `-testactivationheight=blake2b@1`; ordinary regtest without this flag would not test the fork. Nodes use `txindex=1` so relevant historical transaction data remains available to the scanner.

The full node's watch-only wallet is named from the trader's public Nostr identity. RPC imports address-only descriptors with private keys disabled. The separate `faucet` wallet is solely a source of valueless test coins.

The node archives are fetched over HTTPS and checked against the upstream SHA256SUMS. This detects corruption; the current bootstrap does not independently authenticate release signatures. Do not reinterpret this local-development bootstrap as a production binary-verification process.

## Lifecycle commands

```sh
python3 scripts/bootstrap.py             # pinned node downloads, cached
python3 scripts/local.py nodes            # start and initialize both nodes
python3 scripts/dev.py up                 # build/start all services and seed demo balances
sh scripts/build-mac.sh                   # build and ad-hoc sign native app
open bin/Blakeswap.app

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

Config contains `name`, `mode` (`trader` or `tower`), absolute data/password/socket paths, one to three relay URLs, both node RPC URL/cookie paths, and selected tower public identity, payout scripts, and basis-point quote. The tower's status returns its public quote and derived payout scripts for trader configuration. Do not change tower identity or scripts mid-swap: accepted terms and signed templates intentionally retain their original commitment.

## Local API

One newline-terminated JSON object is sent per Unix socket connection:

```json
{"method":"offer.create","params":{"sell":"btc","sell_amount":1000000,"buy_amount":2000000,"tower_bps":50}}
```

The response is either `{"result":...}` or `{"error":"..."}`. Amounts are integer satoshis of the specified chain. The API is versioned by protocol/state v1; remote wallet access is not supported.

| Method | Parameters / behavior |
| --- | --- |
| `status` | Public identity, addresses, available balances, chain heights, signed-book projection, swaps, pending delivery count, provider quote/errors |
| `offer.create` | `sell`, `sell_amount`, `buy_amount`, `tower_bps`; optional Unix `expires`, default 24h |
| `offer.cancel` | Own unreserved offer `id`; publishes cancellation and rejects stale takes |
| `swap.take` | Offer `maker` public key and `id`; creates a secret and durable encrypted request |
| `pause` | Boolean `paused`; stops protocol/tower advancement but not chain time |
| `wallet.recovery` | Explicitly returns recovery phrase and backup caveat; never use in logs |
| `wallet.backup` | Saves a consistent encrypted backup in that profile's data directory |
| `regtest.faucet` | `chain` and `amount`, bounded to 10 test coins; caller's deposit address only |
| `regtest.mine` | Optional `chain`, otherwise both; `blocks` 1–200, default 2 |

Examples:

```sh
python3 scripts/dev.py call alice offer.create '{"sell":"btc","sell_amount":1000000,"buy_amount":2000000,"tower_bps":50}'
python3 scripts/dev.py call alice regtest.mine '{"blocks":2}'
python3 scripts/dev.py call alice wallet.backup
```

There is no `execute arbitrary PSBT`, `sign arbitrary hash`, remote withdrawal, or forced cancellation of a funded swap endpoint. Posting an offer authorizes the daemon to accept its exact terms and fund after required validation; the UI explains this before publication.

## Backups

Use **Wallet → Save encrypted state backup** or `wallet.backup`. The snapshot is encrypted with the profile's current vault password; it includes the seed and pending-swap data. Store the matching password separately and securely. The mnemonic alone cannot reconstruct a random swap preimage or every previously issued rescue signature.

For recovery of this local test environment, stop the affected daemon, preserve its current database, and restore the matching backup to `state.db` with mode 0600. Keep the matching `vault.password` and same chain data/configuration. Start the daemon and inspect both-chain observations and every pending swap before relying on it. Do not copy a vault while a writer is active; use the consistent backup command.

Restoring stale snapshots is not generally safe automatic recovery. Previously shared signatures and preimages remain valid even if the snapshot predates them. See the explicit [rollback limitation](RISKS.md).

## Troubleshooting

**Daemon disconnected:** run `python3 scripts/dev.py up`, inspect `.local/<name>.log`, and check the configured absolute socket path. A locked desktop does not stop the daemon; a sleeping/offline machine can stop timely responses.

**Insufficient balance:** use the test faucet from Wallet, then mine two blocks. Confirmed available balance excludes unconfirmed change and locked HTLCs. Multiple open offers can overstate available inventory; reservation is serialized and funding still verifies actual unspent coins.

**Awaiting durable tower receipt:** ensure the selected tower and at least one shared relay are running. The daemon will not fund a protected leg based solely on a relay acknowledgment. Expired headroom may require a fresh offer/swap rather than continuing stale terms.

**Awaiting chain confirmations:** mine blocks on the relevant chain. Avoid advancing the chain arbitrarily while participants are negotiating, because locktimes do not pause.

**Secret reveal cutoff reached:** the honest daemon refuses first revelation. It continues watching and allows its own refund when eligible. The counterparty may also refund, and configured towers become eligible after their refund grace.

**Contested outcome:** inspect both spend IDs and chain histories. A claim/refund split reflects violated liveness/security assumptions, not a state that the relay or a UI cancellation can reverse.

**Relay history/storage limit:** the local implementation fails visibly at bounded capacities. Archive/rebuild a disposable regtest environment only after completing or safely recovering all swaps. There is no production retention/compaction workflow.

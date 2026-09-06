# Architecture

## Components

| Component | Responsibility | Keys and authority |
| --- | --- | --- |
| SwiftUI macOS app | Market, exact-amount offer form, swaps, wallet, recovery controls, local test mining | No standing wallet key storage; recovery phrase is displayed only on explicit request |
| Go trader daemon | Derivation, signing, order projection, negotiation, chain verification, durable state, rescue scheduling | Its own spending keys and Nostr identity; never the counterparty's keys |
| Nostr relays | Store signed public offers and opaque persistent gift wraps; support WebSocket subscriptions | No spending authority and no private swap plaintext |
| Watchtower daemon | Validate and persist fixed rescue templates; acknowledge jobs; scan both chains; insert public preimages and broadcast after delay | Nostr identity and its fee wallet; no trader private keys or undisclosed swap preimages |
| Bitcoin Core node | BTC consensus, mempool, blocks, RPC, watch-only address observations | Faucet keys for regtest only; trader wallets imported as address descriptors with private keys disabled |
| Bitcoin Blake2b node | Actual fork consensus and unified signatures, v2 block headers, mempool and RPC | Same watch-only separation; a distinct chain/datadir |

The desktop owns a Go helper with an independent engine for each saved wallet on
the selected network. Settings creates wallets and edits their display names. The CLI can instead run
independent trader and tower daemons. Every wallet engine also accepts and advances watchtower jobs alongside its own swaps. Public listing is opt-in; a shared npub supports encrypted private discovery. Each connects to external chain services and
one or more Nostr relays. The maker serializes reservations of its own offers;
there is no authoritative matching database or service holding user funds.

By default wallets connect to public Electrum servers. Settings also accepts
user-operated full-node RPC backends.

## Private local API

The SwiftUI client uses generated SwiftProtobuf messages and gRPC Swift 2 over a
private Unix socket. The daemon also serves an authenticated loopback HTTP gateway
from the same protobuf service with generated OpenAPI documentation. File-based
startup bearer credentials protect both transports; browser Origins and foreign
Hosts are rejected. See [API](API.md) for the exact contract and limits and
[Packaging](PACKAGING.md) for process ownership and shutdown.

Wallet mutations and protocol advancement are serialized. Immutable public
status/Settings snapshots remain readable during slow external IO. Settings use a
revision-based compare-and-swap update and persist atomically. Network changes
are blocked while local offers or swaps remain active, even if the current node
is unreachable. Connection changes preserve wallet state and immutable swap terms.

## Key derivation and persistence

A 256-bit entropy BIP-39 mnemonic produces one BIP-32 master key. All child derivations are hardened:

```
m / 83696968' / branch' / context[0]' / ... / context[7]'
```

Branch `0` is BTC, `1` is Blake2b, and `2` is the application's Nostr identity. The context is SHA256 of `blakeswap/v1/` plus a purpose string; its eight big-endian words have their high bit cleared before hardened derivation. This leaves 248 bits of context separation. Current purposes are `deposit`, `swap/<random 256-bit swap ID>`, and `nostr-identity`. Mainnet and testnet prefix each purpose with `<network>/`; regtest retains the original purposes for recovery compatibility. A desktop profile shares one encrypted master mnemonic across its isolated network databases, while addresses, Nostr identities, and swap keys differ by network.

Each chain gets distinct deposit and per-swap keys. No extended public keys are disclosed. The current wallet uses a stable deposit/change address per chain, which has privacy costs. It does not claim to be a standard external wallet path or allocate a registered coin type.

Snapshots include the mnemonic, preimages, accepted immutable terms, raw signed transactions, tower jobs, receipts, inbox deduplication records, and the outbox. They are JSON encoded, encrypted with AES-256-GCM using fresh random nonces, and committed atomically by bbolt. A random 32-byte salt and scrypt (`N=32768, r=8, p=1`) derive the key from the vault password. Authenticated associated data binds the state format. Backups copy the consistent encrypted database.

Both the desktop and local launcher create a random password in a separate `0600` file. This is not Keychain-backed storage. Someone who obtains both files can decrypt the wallet. See [Risks](RISKS.md).

## Chain boundary

Both chain backends support real transactions on mainnet, Testnet4, and regtest.
RPC accepts explicit loopback HTTP or HTTPS, authenticates from a local cookie
file, requires wallet/transaction-index support, and checks genesis, network name,
header format, and active Blake2b deployment. Electrum uses TLS for public servers
or plaintext only on literal loopback, with CA validation or an explicit certificate
pin. It checks genesis, the fork checkpoint/rule set, header format and individual
proof of work within the network limit, raw transaction IDs/outputs, and merkle
inclusion. Confirmation counts require a continuous sequence of headers from the
transaction's block to the subscribed tip, fetched in bounded batches; validated
batches are cached with endpoint checks before resuming interrupted downloads.
Confirmations are returned only after the full range connects. Median time requires eleven
linked headers and a stable tip. It queries script
history for relevant confirmed and mempool spends, and detects changed headers
during observations. It does not validate the full difficulty/chainwork history:
canonicality, completeness, and availability remain trust in the configured
operator. [Risks](RISKS.md) details the difference from a user-controlled full node.

BTC funding additionally requires bounded ancestry to a mature, post-fork BTC
coinbase that differs from the Blake2b coinbase at the same BIP-34 height. The
check runs before own BTC funding and before accepting counterparty BTC funding.
Separate derivation paths alone are not replay protection.

Go constructs and signs native SegWit funding and spend transactions locally. Nodes receive signed transactions and watch-only address descriptors. BTC signatures use BIP-143 `SIGHASH_ALL`; Blake2b signatures use `SIGHASH_ALL|UNIFIED` (`0x21`). The fork signer is tested against upstream vectors and the actual fork node.

Confirmed funding is checked by outpoint, amount, exact script, and minimum confirmations. The scanner tracks relevant spends in blocks and the mempool, detects tip replacement, reconstructs observations after reorgs, and derives current confirmation counts. Preimage knowledge is saved separately and never rolls back when a revealing transaction disappears.

## Nostr boundary

Public offers use experimental addressable kind `38481` with `d=<offer ID>` and `t=blakeswap-<network>-v1`. This is not a registered general-purpose event kind or NIP-69 fiat order. Private application rumors use kind `10481`, encrypted inside a NIP-59 seal (`13`) and persistent gift wrap (`1059`) using NIP-44 v2. Outer keys are freshly generated and timestamps are randomized up to two days into the past.

Private envelopes also bind the selected network namespace. Every layer checks event ID, signature, kind, recipient, and author binding. The recipient refuses a rumor whose author differs from the seal's signer. Relays see recipients and traffic timing, and may retain ciphertext indefinitely; this is not perfect anonymity or forward secrecy.

Event IDs use a small bounds-checked NIP-01 canonical serializer with known-event and independent Unicode/escaping fixtures. The pinned Nostr library's optimized serializer fails Go's pointer-check instrumentation, so the application does not call that serializer or its event-signing wrapper. It continues using the library's NIP-44 encryption and btcec's Schnorr primitives; runtime race/pointer checks stay enabled.

The local configuration explicitly names one to three relays used by all parties. Dynamic NIP-17/NIP-65/NIP-10050 relay discovery and Tor routing are not implemented. The daemon replays persistent history on synchronization, deduplicates by authenticated sender and application message ID, and acknowledges processing separately from relay storage. Public ordering follows NIP-01 timestamp and ID tie rules, not a global sequence supplied by any relay.

Watchtower announcements use experimental addressable kind `38482`, with both
`d` and `t` bound to the network namespace. Provider signatures bind its identity,
npub, generated P2WPKH scripts, basis-point fee, expiry and public-listing flag.
Public announcements refresh every fifteen minutes and expire after an hour.
An opt-out replaces the old public event with a signed `public=false` event.
Unlisted providers answer `tower-query` with an encrypted `tower-quote`, carrying
the same signed proof without posting an announcement. Protected offers pin the
proof and terms must preserve it. Directory cache and favorites are bounded and
network-scoped; stale quotes cannot authorize new offers.

Discovery queries and replies expire after fifteen minutes and use a bounded
replay cache separate from durable swap acknowledgements. Discovery failures do
not abort settlement. Remote watchtower jobs use separate scanner cursors and a
bounded scan budget after local swap advancement. New registrations must fit the
current funding horizon. Never-seen, explicitly absent funding transactions retire
after the contract's refund grace; observed funding remains an obligation until
settled. Indexer errors never count as proof that a transaction is absent.

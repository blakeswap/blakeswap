# Limitations, risks, and explicit assumptions

## Supported boundary

Mainnet, Testnet4 and regtest swaps are implemented through external Electrum or
full-node RPC backends. This remains experimental software without an independent
security audit. Successful regtest trades and read-only public-chain checks are
not proof of real-fund safety. Windows, hardware signers, mobile clients,
Lightning, Taproot swaps, partial fills, and shared liquidity remain excluded.
The supplied native build targets macOS 15+ on Apple Silicon.

Finite testing cannot establish that cryptography, consensus, distributed scheduling, or economic incentives are correct in all circumstances. The invariant matrix identifies tested boundaries and deliberately leaves unresolved risks visible. There has been no independent security audit.

## Replay protection is not derivation separation

One seed with different hardened paths separates wallet keys. It does **not** make a transaction chain-specific when its inputs exist on both chains. Depositing into different addresses is helpful operational separation but is not a consensus replay barrier for shared-history coins.

The fork's unified signature protects a Blake2b spend against replay to Bitcoin, because Bitcoin does not validate that signature algorithm. The reverse is not guaranteed: ordinary Bitcoin signatures are still accepted by Blake2b consensus. The real-node tests exercise this asymmetry on otherwise-valid local HTLC spends. The application requires unified signatures for all Blake2b spends and fails closed on the other hash type.

The local demo's funding comes from distinct regtest coinbases. Public BTC funding now requires verified chain-exclusive input ancestry: at least one funding input must descend from a mature post-fork BTC coinbase whose transaction differs from the Blake2b coinbase at the same BIP-34 height. Verification traverses at most 512 transactions and depth 64. It never infers exclusivity from “not found.” This is conservative: legitimate split coins can be refused, especially where an ancestor predates the fork, the other chain has not reached the matching height, ancestry is too large, or coin selection chooses unsuitable coins. The wallet does not implement a coin-splitting service. A confirmed displayed balance is not a promise that these inputs meet replay or fee requirements. A key-path change, address prefix, UI label, or Nostr chain tag cannot replace that work. The domain string also distinguishes application rule sets, not consensus validity by itself.

## Atomicity needs bounded response time

HTLCs protect claim/refund conditions, not unlimited offline time. After the taker reveals the secret, the maker or its tower must get the long-chain claim confirmed before the taker's refund wins. If that does not happen, the taker can receive the short-chain asset and recover its long-chain asset. A malicious participant can exploit expired safety margins; the script cannot compel cooperative behavior.

Regtest uses short block horizons for tests. Public swaps use four/two-day median-time refund deadlines and six confirmations per chain, with clock-skew, freshness, funding and reveal gates. Either chain can stall, manipulate timestamps, reorg, or censor transactions. Incorrect local wall time can prevent new actions. Six confirmations do not represent comparable economic finality on chains with different security budgets; these timeout and confirmation values still require independent calibration.

The reveal cutoff is honest-client policy. A malicious secret holder may reveal late, reducing or eliminating the owner's grace before a fixed tower takeover. Current script does not offer a trustless cross-chain timer starting when a preimage appears. Neither a relative locktime measured from funding nor an absolute locktime can provide that cross-chain observation primitive.

Claims remain valid after refunds become eligible, so settlement can be a race. Observing one confirmed leg is not proof both legs are final. Reorgs can reverse funding or settlement observations. Once revealed, a preimage stays compromised even if the revealing transaction is removed from the mempool or chain.

## Watchtower limitations

- A tower can withhold service, lose storage, disconnect, lie about durability, or disappear. Its signed receipt provides accountability, not enforceable uptime.
- It can broadcast the authorized transaction as soon as eligible and compete with an owner's unconfirmed fee-free claim. “Only if needed” cannot prove who was online or who first attempted to act.
- All pre-signed fee variants may be too cheap. Relay policy, pinning, RBF rules, congestion, and censorship can prevent confirmation before a refund race.
- Only the selected provider is paid. Multiple independently selected providers, bounty auctions, replacement providers, and coordinated fees are not implemented.
- A tower never receives an unrevealed preimage for the taker's first claim. It cannot complete that first-reveal action on behalf of a permanently offline taker. Both parties can instead recover through refund paths.
- The watchtower has its own Nostr and fee-wallet keys, but none of the traders' keys. “Keyless rescue” refers to trader spending authority, not to the absence of all private keys in the provider process.

## Relay and network limitations

Nostr distributes data; it is not a consensus orderbook or proof of reserves. Offers can be stale, duplicated, overcommitted, expired, or visible on only one relay. The maker's local reservation decides which concrete request it accepts. A maker can advertise more than it can fund; takers must still validate the actual contract, and capital can be temporarily locked if the counterparty disappears.

Relays can censor, reorder, omit, retain, or lose messages. Redundant storage and retries improve availability without guaranteeing delivery. Public order status changes cannot revoke a previously shared transaction. Cancellation races are resolved at the maker, and clients may temporarily see an unavailable offer.

The current mailbox sync replays retained history with a 10,000-event bound. The local relay errors instead of silently truncating oversized history, but third-party relays can have different policies. There is no scalable paginated cursor, retention/compaction policy, dynamic relay discovery, Tor routing, peer scoring, or public anti-Sybil mechanism. Long-running public workloads require that additional engineering.

NIP-44/59 encryption does not provide forward secrecy. A later identity-key compromise can expose stored historical ciphertext. Relays still see recipient tags, connection addresses, sizes, and timing. Public offers identify their maker; blockchain scripts/amounts and shared hashlocks can link the two legs. Stable deposit/change addresses make wallet activity linkable. This is not an anonymity system.

For relays requiring NIP-42 authentication to read mailboxes, the daemon proves
control of its network-specific Nostr identity to that configured relay. The relay
can associate that identity with the connection; authentication neither reveals
the secret key nor grants spending authority. The daemon does not authenticate
gift-wrap writes, so providers requiring authenticated publication are unsupported.

## Wallet and local-machine risks

The wallet is a hot software signer. Malware or another process running as the same user may read its memory, access its socket, or read both the vault and its local password file. File permissions and encryption protect different boundaries; they do not defend a compromised user account. The desktop and test launcher store passwords in files rather than macOS Keychain, and the app has no hardware-wallet or biometric signing policy.

The mnemonic restores derivation keys. Pending preimages, exact negotiated terms, prepared refunds, fallback signatures, receipts, and reliable-message state also need the encrypted database. Restore the matching password with a backup. Restoring an old snapshot and blindly resuming may reuse stale order state or omit an already disclosed secret. Reconcile both chains and counterparties before resuming from a stale backup; rollback-safe production recovery is not implemented.

Public Electrum is the default and a material trust dependency. Raw transaction
IDs, scripts, amounts, merkle branches, fork identity and individual header proof
of work against the network limit are checked. Confirmation proofs link every
header from the observed block to the subscribed tip, and median-time reads link
their eleven headers. Cold reads of old transactions can require substantial
header downloads. The wallet does not build and validate the full header
chain, difficulty transitions, or most-work selection. A malicious indexer can
omit spends or histories, present a stale/private branch, or misrepresent which
valid block is canonical. That can undermine confirmation checks, replay ancestry,
and timely preimage detection. One server per chain is configured; there is no
multi-operator quorum or automatic server failover. For consensus validation under
your control, configure your own fully validating node. An RPC server is also a
trusted observation source if you do not control it.

TLS authenticates an endpoint, not its chain claims. The shipped Blake2b service
uses a self-signed certificate whose observed DER fingerprint is pinned; initial
fingerprint acquisition is a trust-on-first-observation limitation. Pin mismatch
or expiry stops connection and requires a deliberate verified update. Public
service operators can disappear, rate-limit or change policy. No verified public
Blake2b Testnet4 indexer is shipped; an empty setting requires an explicit own
endpoint. Public relays may reject the application's experimental offer kind or
limit gift-wrap retention even if their health check succeeds.

The wallet supports a narrow P2WPKH coin-selection path with bounded input counts and fixed funding fees, and P2WSH contract spends. Dust, insufficient confirmed balance, or an insufficient fee allowance may stop execution. It has no automatic deposit withdrawal/consolidation workflow, external signer negotiation, or general PSBT importer. The UI's confirmed balance excludes locked contracts and unconfirmed change; the Swaps screen shows locked principal separately.

## Remaining work before considering real funds

At minimum: independent protocol and implementation audits; broader replay-safe coin selection/splitting support; per-chain security calibration; crash/fault injection at every durable boundary; rollback-safe recovery; adaptive bounded fees and pinning analysis; public-relay interoperability and cursor/retention design; provider incentives/admission; hardened key storage; privacy improvements; and broader platform/packaging verification. These remain unresolved by successful local-regtest demonstrations or public endpoint read checks.

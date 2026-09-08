# Verification and invariant matrix

## Reproduce

```sh
sh scripts/test.sh
```

The script performs static analysis, package tests with the Go race detector, the pinned NIP-44 library's published-vector tests, two bounded fuzz campaigns, actual two-chain integration tests, coverage output, and a native macOS build. Regtest tests are intentionally serialized across packages with `-p=1`, because they mine and invalidate blocks on shared local nodes. Do not run concurrent manual swaps while a timing-sensitive harness is manipulating those chains.

For a focused run:

```sh
sh scripts/go.sh test ./internal/protocol ./internal/transport ./internal/storage
BLAKESWAP_REGTEST="$PWD" sh scripts/go.sh test -p=1 ./internal/contract ./internal/daemon -v -count=1
```

Without `BLAKESWAP_REGTEST`, real-node cases explicitly skip; a passing unit run is not evidence of a two-chain integration pass. The complete script initializes actual upstream nodes first. It never substitutes a mock chain or a second unmodified Bitcoin node for Blake2b.

## Invariants

| ID | Invariant / boundary | Evidence |
| --- | --- | --- |
| C01 | BTC and Blake2b are initialized regtest networks with the expected rule sets | Every real-node test calls startup identity checks, header-width checks, and active Blake2b deployment verification |
| C02 | Blake2b signatures match the actual fork algorithm | All eight applicable upstream ALL/SegWit-v0 vectors; actual node accepts and mines locally signed funding, claims, and refunds |
| C03 | Unified replay protection is one-way | Real BTC node rejects a unified signature on its otherwise-valid HTLC; real Blake2b node accepts an ordinary BTC-style signature; application policy still rejects wrong-chain hash types |
| C04 | A claim needs the correct 32-byte preimage and signature | Wrong-secret and wrong-branch tests; real-node rejection; extraction and malformed input fuzzing |
| C05 | Owner/tower signatures bind outpoint, input count, sequence, version, locktime, payouts, and destinations | Mutation matrix across both signature algorithms; actual early-locktime/payout mutation rejection |
| C06 | Tower claim cannot confirm early | Real-node `testmempoolaccept` returns `non-final`; daemon takeover checked one block before eligibility and at eligibility |
| C07 | Owner confirmation prevents later bounty spend | Both nodes mine owner claim, reject competing fallback, verify consumed HTLC and zero bounty |
| C08 | Refund cannot confirm before its threshold | Local constructor and actual node reject early refund; eligible refund confirms on both chains |
| C09 | A confirmed fallback pays only the pre-authorized bounty | Real delayed tower transaction, exact percentage assertion, signature-bound payout mutation rejection |
| C10 | Each HTLC output is consumed once | Real UTXO disappearance and conflicting-spend rejection after every settlement path |
| P01 | Terms preserve signed order, price, assets, keys, hash, domains, and policy | Authenticated offer validation and contract-field mutation table |
| P02 | First reveal requires confirmed outputs and remaining margins on both chains | End-to-end funding phases; exhaustive 111 × 61 local-height boundary grid for each action gate |
| P03 | Refusing late revelation must still permit self-refund | Regression scenario advances only the long chain past its deadline; taker with no tower refuses revelation and successfully refunds while maker is offline |
| P04 | One whole offer is reserved at most once | Two distinct take requests race; exactly one maker swap exists and the other gets authenticated rejection |
| P05 | Local cancellation defeats a stale unreserved take | Cancellation commits before delivery of a previously read offer request; no maker swap/funding is created |
| P06 | No protected funding before exact tower receipt | Funding waits while provider is absent; forged sender and altered job digest receipts are rejected |
| P07 | Either market direction is supported | Real BTC-sell and Blake2b-sell swaps, including explicit no-tower operation |
| P08 | Users need not be online simultaneously | Harness closes and reopens trader daemons from encrypted databases at handoffs; relay is their only mailbox |
| P09 | Owner offline after revelation can be rescued | Maker stopped before first reveal; tower sees mempool secret, survives restart, waits until takeover, claims, and returning maker records completion |
| P10 | Both parties offline without revelation can refund | Both stopped after funding; no secret known to tower; both delayed refund jobs confirm and both traders recover from disk |
| P11 | Reorgs demote settlement; secret knowledge never reverts | Real claim block invalidation, changed confirmation state, retained secret, re-mining, and completion recovery |
| N01 | Nostr signatures and event IDs are independently validated | Altered content, false ID, future event, wrong kind, and forged order tests |
| N02 | Private content is encrypted for the intended recipient | NIP-44 vectors, wrap/unwrap roundtrip, wrong-recipient failure, one-time outer identities, ciphertext plaintext check |
| N03 | Rumor author must match seal author | Adversarial correctly encrypted envelope with mismatched inner author is rejected |
| N04 | Relay history survives restart and old orders do not resurrect | Persistent relay restart; new cancellation plus stale re-publication; deterministic same-time ID tie test |
| N05 | Relay mailbox authentication binds the identity, challenge and URL | Local WebSocket handshake tests cover required and unsolicited authentication, missing challenges, rejected/malformed/unrelated acknowledgments, partial history discard, and bounded retry; opt-in public reads exercise actual orderbook/mailbox filters with disposable identities |
| N05 | Message duplicates are idempotent and IDs cannot change meaning | Actual daemon receives same gift wrap twice, reserves once, rejects different signed contents under same application ID |
| N06 | Relay acknowledgment is distinct from recipient processing acknowledgment | Durable outbox survives restart; forged application ack cannot clear delivery; authenticated intended-recipient ack clears it |
| K01 | Chain, swap, and messaging keys are separate and recoverable | Hardened derivation separation and repeat mnemonic recovery tests |
| K02 | Secrets are absent from public status and stored encrypted | Public status checked against actual mnemonic, private identity, and secret; encrypted file plaintext scan |
| K03 | Vault authentication, backup consistency, and writer exclusion | Wrong password rejection, second-writer rejection, preserved secret after encrypted backup/reopen, mode 0600 assertion |
| E01 | Exact integer money, bounded quote, and non-dust payouts | Decimal-to-satoshi edge cases, overflow rejection, bounty rounding and economic-floor tests |
| U01 | Native UI operates the real daemon | Manual automated native-app exercise: create offer as Alice, switch to Bob, take, mine through the GUI, observe both claims with two confirmations |

## Real demonstration evidence

The first daemon-driven trade completed with swap ID:

```
e6792b968263832054bb592c70e57694e4cd0f13c4827bb213b81c72b410e5b9
```

The native macOS interface then created and took another offer. Its swap ID begins `a982af1abd7992e1`; both chain claims were observed in the native interface at two confirmations. Public transaction details are retained in the local daemon status; `.local/successful-trade.json` contains the CLI demonstration's full result. These IDs refer to this particular local chain history, not public explorers or portable fixtures.

Integration tests log fresh funding/claim/refund IDs and bounty amounts for each run. The tower takeover case confirmed a 10,000-sat bounty on a 2,000,000-sat principal at 50 basis points. Self-claim cases confirmed zero tower bounty. Test reports contain no mnemonic or private preimage.

A further trade through the final native build completed on September 5, 2026. See [the final verification record](VERIFICATION.md) for its complete funding/claim IDs, independent node checks, and recorded harness results.

## What this suite does not prove

- It is not a formal proof over every message schedule, chain fork, key compromise, consensus bug, or adversarial economic strategy.
- The height grid exhausts its finite specified boundary range, not arbitrary future mainnet conditions.
- Real process restart tests cover durable handoffs and provider recovery. They do not inject power loss or disk corruption at every machine instruction/fsync boundary.
- Fuzzing is time-bounded and seeds important transaction/envelope parsers; it does not establish absence of all parser bugs.
- Claim reorg coverage is real but bounded. It is not an exhaustive analysis of deep funding reorgs, two competing mining networks, eclipse attacks, or mining censorship.
- Current fixed fee ladders are tested for signature/bounty integrity and successful local confirmation, not viability under arbitrary mainnet congestion or pinning.
- The race detector covers executed Go paths; it does not analyze protocol-level economic races or native SwiftUI code.
- Native UI verification is a recorded end-to-end exercise, not a packaged XCUITest suite across macOS releases or display sizes.

See [Risks](RISKS.md) for the trust and liveness assumptions that remain necessary even when every test passes.

## API, desktop and public-backend coverage

| Boundary | Evidence |
| --- | --- |
| Protobuf maps full HTLCs and exact int64 money | Native gRPC and HTTP round-trip tests include values above JavaScript's exact integer range, amount, outpoint and timestamp locktime |
| Local API authorization | Missing token, foreign Origin/Host, private file modes, startup token discovery and shutdown credential cleanup |
| Settings persistence/isolation | Revision conflict, invalid endpoints/network sets/relays, encrypted offline active-swap guard, stable profile master seed, readable snapshots during external IO |
| RPC history readiness | Completed descriptor imports survive reconnect without rescan; interrupted/partial scans stay unavailable; bootstrap cancellation releases vaults before network checks |
| RPC mempool observation | Repeated cycles query only watched outpoints and preserve the revealed secret despite 10,000 unrelated transactions |
| Native snapshot isolation | Network mismatch, stale Settings revision, profile round trip and invalidated settings-save generation cannot publish old addresses |
| Public timing | Both assets as maker sell side, asymmetric chain heights, exact funding/reveal boundaries, clock skew/staleness, malicious far-future schedules |
| Consensus timestamp finality | Real BTC and Blake2b nodes reject delayed claims/refunds at exactly MTP=locktime and accept after MTP advances one second |
| Electrum transport | Invalid JSON, response ID confusion, missing results, explicit missing-transaction classification |
| Indexer observation integrity | Real headers/transactions with forged genesis, raw transaction, merkle branch, UTXO amount and duplicate UTXO replies rejected |
| Header observation continuity | Network PoW limits for both header formats, disconnected confirmation/median-time ranges, malformed/short batches, tip replacement, cached-range reorgs and resuming interrupted downloads |
| Funding publication durability | Both roles crash before/after node acceptance, reopen past funding deadlines and confirm refunds; legacy snapshots reconcile observed funding while missing/error lookups cannot authorize late funding |
| Actual Electrum swaps | All five asynchronous/restart/self-claim/tower/refund/reorg scenarios run through local Electrum fixtures indexing real regtest blocks |
| Replay boundary | Shared/pre-fork ancestry rejected, absent opposite-chain coinbase never treated as exclusivity, mismatched transaction IDs/cancellation fail closed |
| Network identity | Fork v2 header hash fixture from upstream, checkpoint/hash tampering, cross-network signed offer/mailbox rejection and key derivation separation |

The Electrum fixture is test-only Go code. It forwards broadcasts to actual
regtest consensus nodes and constructs inclusion proofs from their blocks. Real
public endpoint reads are separately opt-in:

```sh
BLAKESWAP_LIVE_READS=1 sh scripts/go.sh test -count=1 -run TestPublishedElectrumServices -v ./internal/chain
```

No public event posting or real-money trade is part of the automated harness.
Run `BLAKESWAP_TEST_ELECTRUM=1` with `BLAKESWAP_REGTEST` for the Electrum daemon
matrix. `BLAKESWAP_BTC_RPC_PORT` / `BLAKESWAP_BLAKE_RPC_PORT` isolate test ports,
and `BLAKESWAP_RDTS=1` starts the external Blake2b fixture with reduced-data rules
active. The suite mutates test nodes and runs chain packages sequentially; do not
run independent mining suites against the same datadirs concurrently.

The DMG checks include signature/resource sealing, disk-image checksums, relocated
app launch without repository dependencies, and process-tree inspection for exactly
one app-owned Go helper. GUI shutdown and parent
death must remove the helper/runtime while external fixtures remain alive.

The GitHub Go validation workflow runs vet, unit/IPC lifecycle tests, race checks,
NIP-44 vectors and formatting on Linux. The separate version-tag/release workflow
defines native Swift and DMG checks on both Mac architectures. Actual two-chain
integration still requires separately prepared local fixtures; neither ordinary
Go CI nor the macOS release workflow sets those up.
The native client has a separate actual gRPC trade test using the same DaemonRPC
implementation as the GUI:

```sh
python3 scripts/desktop-demo.py prepare
BLAKESWAP_SWIFT_TEST_ROOT="$PWD/.local/desktop-demo" sh scripts/test-swift.sh
```

Run it after the Go integration suite, not concurrently on shared test nodes.
It starts only a test wallet daemon, uses external node/relay fixtures, and saves
public settlement evidence in `successful-swift-trade.json`.

Run the native snapshot regressions without chain services:

```sh
swift test --package-path macos --scratch-path .cache/swift-build --cache-path .cache/swift-cache -c release --filter AppModelTests
```

## Market and watchtower regressions

Rescue fee regressions cover per-network persistence, bounds, propagation to all
wallets, legacy defaults, immediate public/private quote replacement, and retained
accepted jobs and receipts after a rate change and restart. Native Settings tests
cover fee serialization and independent chain readiness; desktop snapshots clear
partial heights on failure or network changes. Missing RPC cookie tests check
actionable connection guidance. Run `--filter SettingsTests` with `swift test` for
the native settings regressions.

Native tests cover fee-inclusive sell balances, zero/unknown balances, amount
bounds, all/own/other open-order filters, and automatic helper restart with no
relaunch during shutdown. The native gRPC trade also checks that backend error
messages reach the UI and that generated watchtower scripts/npub are exposed with
public listing off by default.

Go tests cover provider signature, identity/network/expiry/payout/visibility
binding, encrypted private npub lookup without a public event, public opt-in and
withdrawal ordering, per-network favorite persistence and normalization, and
pause rejection. `TestRealDiscoveredTraderWatchtowerAndOfferBalance` uses actual
BTC/Blake2b transactions: it rejects unfunded offers, pins a privately discovered
quote, receives a durable job receipt from a trading wallet, reopens that wallet
from disk, and confirms its delayed refund rescue.

Additional regressions isolate recurring discovery from a saturated protocol
mailbox, expire discovery state, reject invalid-curve npubs and out-of-range
observed outputs, keep remote history out of the local swap scanner, and distinguish
abandoned registrations from funded obligations and failed indexer lookups.

Wallet regressions cover legacy Alice/Bob migration without seed replacement,
immutable IDs, invalid/deleted profiles, and creating/renaming a wallet through
the real IPC service while nodes are unavailable. Native tests exercise custom
wallet selection across networks and create a third independent wallet, reject
its empty-balance offers on both assets, and verify its addresses/identity survive
renaming before completing an actual two-chain trade. Watchtower tests also cover
asymmetric public clocks, unrelated funding-output rejection, and one private
lookup per provider per expiry period after successful relay publication.

## Onboarding and reset regressions

`AppStartupTests` holds a real helper before it creates its runtime manifest and
verifies that the opening screen stays in its loading state, then receives
settings in the same refresh when the helper becomes ready. Additional cases
cover startup timeout, helper exit, launch failure, cancellation, and rejection
of unsafe or invalid runtime files. Run with `BLAKESWAP_TEST_HELPER` pointing to
the built desktop helper and `swift test ... --filter AppStartupTests`.

Go tests cover first-launch engine gating, backup confirmation, invalid phrases,
revision conflicts, interrupted wallet installation, completed/legacy setup,
encrypted backup round trips, wrong passwords without source modification, and
retained pending swaps/network guards. Python reset tests cover the actual Make
target with spaces in its data path, preservation of archived files, and refusal
of active locks, symlinks, or unrelated directories. CI and `scripts/test.sh` run
these tests; `make test-reset` runs the reset checks alone.

Native tests create a temporary installation through the real packaged helper
and gRPC client, restart during backup, finish setup, and restore by phrase and
encrypted file. They never contact public services or use existing wallet data:

```sh
sh scripts/build-mac.sh
BLAKESWAP_TEST_HELPER="$PWD/bin/Blakeswap.app/Contents/Resources/blakeswap" \
  swift test --package-path macos --scratch-path .cache/swift-build \
  --cache-path .cache/swift-cache -c release --filter OnboardingTests
```

`scripts/test-swift.sh` includes these tests alongside its configured regtest
trade. AppModel tests also ensure backup completion clears recovery material and
rejects delayed snapshots from an earlier setup step.

## Receive addresses, local node discovery, and release packaging

`TestReceive*` covers confirmed-only per-chain rotation, historical balances,
repeated receipts, restart/reorg monotonicity, spent-history recovery, import
failures, and persistence failure. `TestRealReceiveRotationSpendsMultipleHistoricalAddresses`
broadcasts funding signed by multiple receive keys on both actual chains, checks
change rotation, and reopens a counter-less state. It also runs through the Electrum
bridge when `BLAKESWAP_TEST_ELECTRUM=1`. The contract tests verify both signature
algorithms for mixed-key inputs. Native tests decode generated QR images to the
exact address and check numeric rescue-fee rounding/bounds.

Cookie discovery tests distinguish unreachable nodes from missing registrations,
reload changed registrations, bind credentials to the selected endpoint, preserve
explicit paths, and migrate obsolete generated defaults. `make test-local-nodes`
checks launcher registration and per-chain Make targets. `make test-packaging`
checks tag validation and bundle version metadata. Both run in CI. The macOS
packages workflow builds, verifies DMGs and binary architectures, and runs native
tests on Apple silicon and Intel when a version tag or release is published. It
sets `BLAKESWAP_TEST_HELPER`, enabling startup/onboarding tests, but does not set
`BLAKESWAP_SWIFT_TEST_ROOT`, so the external regtest gRPC trade is skipped. This
describes the workflow, not evidence that a particular release ran successfully.
Pull requests and main-branch pushes keep Go validation in the separate CI
workflow without building DMGs.

## Sends, reservations, and safe request expiry

[Send regressions](../internal/daemon/send_test.go) check durable-before-broadcast
storage, identical retries, locked/spent coin and invalid-fee rejection, persisted
open-order locks, pending request expiry, late acceptance rejection, and safe
maker reservation expiry before prepared funding.
`TestRealSendsHonorCoinControlFeesAndOrderLocks` checks selected inputs, exact fee
and change, and confirmation on both regtest chains; it requires
`BLAKESWAP_REGTEST` and also runs through the Electrum fixture in `scripts/test.sh`.
Native [coin-control tests](../macos/Tests/SendCoinsTests.swift) cover send review
amounts, dust, fees, locked/unconfirmed coins, and wrong-chain selections. These source tests are not
a claim of a new integration run for a documentation-only correction.

## Order privacy and wallet background progress

Privacy regressions inspect public offer content and every status publication,
decrypt peer negotiation to verify it contains no protection fields, test all four
independent on/off combinations, and verify jobs decrypt only for their provider.
Encrypted-state reloads preserve each local choice. Cache cleanup withdraws retired offers and removes stale publication retries
without depending on the old configured provider. Retired public fields are
rejected without version negotiation.
The existing real-chain protection scenarios now explicitly select both wallets’
providers and still verify receipt-gated funding, takeover, refunds, and reorgs.

Worker regressions hold one wallet’s chain operation while another continues
background cycles, publishes status, and responds to manual refresh. They cover a
refresh arriving during an older cycle, stale network bindings, and worker shutdown.
Native tests verify maker-only protection labels and complete a two-chain swap
while polling only Bob, then verify the manual refresh result after mining.

Available-funds regressions verify the non-overlapping deposit partition,
overlapping reservation owners, signed-send retention, pending change,
confirmation and reorg transitions, separate contract principal, and unavailable
contract observations. Preflight tests cover live output rechecks, reservations,
duplicate inputs, wallet/network isolation, bounded concurrency outside the
settlement mutex, and retry after backend failure. Replay tests explicitly cover
mixed shared/exclusive sets, shared ancestry, depth exhaustion, backend errors,
and changed canonical coinbases without cached evidence. Native tests check
positive total balances with zero unlocked funds and bind readiness to exact
wallet/network/form/input context. `TestRealFundsContractPrincipalAndPendingChange`
and `TestRealSendsHonorCoinControlFeesAndOrderLocks` exercise these categories and
preflight with actual BTC and Blake2b nodes; run the latter with the Electrum
fixture as well.

## Fee policies and replacement recovery

[Fee tests](../internal/chain/fees_test.go) verify exact bounded parsing for both
backend methods, effective RPC targets and stale/unavailable responses. Contract
size tests cover 1, 2, and 50 inputs with P2WPKH/P2PKH/P2WSH output lengths. Daemon
regressions cover fee consent, dust and limits, duplicate bumps, persisted variants,
ambiguous broadcasts, restart, earlier-variant confirmations and deep reorgs.

`TestRealSendFeeAccelerationBothChains` exercises below-relay rejection, restart,
authorized replacement, RBF, confirmation, invalidation and reconsideration on
actual BTC/Blake2b. `TestRealSettlementFeeVariantsKeepFundingAndPayoutAuthorization`
checks all three claim/refund/tower fee tiers and preserves signed original
recovery and payout invariants. Set `BLAKESWAP_TEST_ELECTRUM=1` for the daemon's
Electrum matrix. These require the exclusive isolated regtest fixture; ordinary
unit passes with skipped real tests are not two-chain evidence.

## Reviewed trade acceptance

`TestTrade*` daemon tests cover read-only cancellation, exact per-asset outcomes
reconciled against owner/tower transaction constructors, signed order/provider
changes, wallet/network/fee/expiry/input binding, concurrent confirmation, and
pending/accepted/rejected identities across encrypted snapshot reloads. A quote
cannot authorize a second request ID. Typed API mapping includes amounts above
JavaScript's exact integer range.

`TestRealReviewedSwapThroughTypedAPI` creates and takes both market directions
through authenticated generated gRPC clients, restarts each daemon immediately
after confirmation, completes the automatic swap, and compares confirmed owner
receipts with the reviewed bounds. Run it against the isolated two-chain fixture,
serially with all other node-mutating tests; set `BLAKESWAP_TEST_ELECTRUM=1` for
the actual-chain loopback Electrum bridge matrix:

```sh
BLAKESWAP_REGTEST="$PWD" sh scripts/go.sh test -p=1 ./internal/api -run TestRealReviewedSwapThroughTypedAPI -v -count=1
```

`TradeReviewTests` exercises native maker/taker orientation, cancellation, wallet/
network/generation changes during delayed replies, double-click suppression,
ambiguous response and expired-quote restart retries, definitive rejection, and
private minimal journal permissions/overwrite protection. It injects typed
responses into the production review model; it is not a claim of automated
pixel-level UI coverage. Build the bundle and use its helper for startup tests as
described by `scripts/test-swift.sh`.

## Endpoint interruption and isolated recovery

`TestFailover*` covers ordered routing, timeout budgets/backoff, wrong-network
admission failure, stale/conflicting tips, recovery after a legitimate reorg,
proof errors, TLS pin mismatch, watch-history provenance, cancellation and
identical retry after an ambiguous broadcast. `TestIsolated*` covers private
secrets and crash-before-first-broadcast claims, persistence of witnessed
preimages, missing target scans/outputs, healthy-chain signed-send progress,
and permanent refund suppression after an observed incoming claim is reorged.
Settings/API/native tests preserve ordered fallback fields and distinguish stale
values from current readiness.

Integrated fee regressions preserve estimate provenance, owner caps, signed
variants and destinations through failover. Manual acceleration cannot publish a
privately saved claim during a peer outage. Witness-ordering regressions reopen
the encrypted state immediately after a failed funding lookup or rejected refund,
then remove the witness and verify that refund suppression remains durable.
Terminal-history tests preserve completed/refunded rows and network switching
during unrelated outages, while fresh reorg evidence reopens the obligation.
Owner/tower fee selection must recheck source freshness before broadcasting.
Accepted-scan tests change source readiness during/between chain scans and verify
durable immutable witness knowledge. Tower tests observe only the peer chain,
reopen the vault, remove the witness and recover using the target alone.
`TestRealIsolatedTowerWitnessRecovery` repeats that handoff and revealing-block
reorg against actual BTC/Blake2b targets through RPC and Electrum fixtures.
A repeated-tick tower budget regression stalls BTC scanning and verifies that
an eligible Blake claim still progresses while a Blake refund remains held.
Refunds require successful current scans of both chains, including after source
changes. The overall worker deadline continues to stop publication.
`TestPublication*` covers peer changes during fee selection, the second spend
scan, variant lookup and the last maker-funding lookup.
`TestFailoverBroadcastGuardRechecksAfterEndpointSwitch` verifies that admission
of a fallback cannot bypass the publication requirements of its caller.
`TestFailoverIncrementalScanKeepsHealthyEndpointAvailable` distinguishes RPC
catch-up with completed-block progress from a stalled scan, including a reset
cursor. Partial observations remain unavailable, wallet refresh stays usable,
and the same source finishes its retained scan on a later cycle. Both local and
tower entry points are exercised. `TestIsolated*IncompleteScan*` uses real RPC and
Electrum decoders with local fault servers: a valid public preimage precedes a
later timeout, transport/mempool/history error, inclusion error or reorg check.
Confirmed raw replies include block hashes in the header-failure scenarios;
owner and tower witnesses must be saved before the following header lookup,
including when a mempool spender becomes confirmed between reads.
Reloading the vault and removing the witness must still block owner refunds and
permit authorized tower recovery only after a complete target scan. Sink failure
and fallback-attempt tests cover the durability boundary. The real tower test
forces a short initial RPC catch-up slice, asserts publication remains held and
wallet readiness survives, then requires bounded, paced completion.

With the exclusive isolated BTC/Blake2b fixture available, run:

```sh
BLAKESWAP_REGTEST=/path/to/isolated-fixture \
  sh scripts/go.sh test -count=1 -p=1 -run 'TestRealEndpointFailover|TestRealIsolated' -v ./internal/daemon
BLAKESWAP_TEST_ELECTRUM=1 BLAKESWAP_REGTEST=/path/to/isolated-fixture \
  sh scripts/go.sh test -count=1 -p=1 -run 'TestRealEndpointFailover|TestRealIsolated' -v ./internal/daemon
```

The RPC tests inject HTTP endpoint outages; the Electrum tests close actual TCP
connections. Both fixtures use real node consensus and no public funds. Cases
cover settlement in both directions after primary loss, blocked first
revelation during a peer outage, persisted-witness claims while only the target
chain is reachable, and refunds held until peer observation returns. Run the
existing asynchronous settlement/refund/reorg matrix as a regression too.

The Electrum real-chain harness waits for a successful tick and fresh observations
of both chains after switching to a new local indexer. A cold bridge can exceed
an initial request budget while building history; setup logs that cause and
requires readiness within 20 seconds before creating an offer. Trade, refund and
settlement assertions and production timeouts remain unchanged.

The typed trade API fixture uses the same bounded both-chain startup condition
after installing new Electrum endpoints. Quote/cancel comparisons retain every
wallet, balance, reservation, readiness and source-generation field; only endpoint
`last_success` is normalized because successful advisory reads update that clock.
A focused comparison test verifies that other state changes remain failures.

## Activity history and export

`TestActivity*` checks durable IDs, spent/rotated deposits, linked change without
duplicate totals, missing creation-time migration, earlier replacement variants,
reorg demotion, frozen pagination/FIFO eviction, capacity failures, exact CSV and
formula escaping. Lifecycle tests hold history reads outside the engine lock,
close/cancel/join them, and reject late wallet/network/source-generation replies.
`TestActivityBudget*` retains the 200ms per-chain deadline and eight-attempt cap
while checking that rows and signed variants cut off by earlier reads receive a
fresh slice. It covers permanently unavailable rows, unchanged observation ages,
source changes, explicit block contradictions, cancellation, and claim witnesses
persisted before a partial read returns.
Chain tests verify historical receipt APIs rather than substituting current UTXOs.

`TestRealActivityHistoryThroughTypedAPI` exercises both assets through generated
authenticated gRPC: spent old-address deposits, exact send replacement fees/change,
restart, a new deposit during frozen paging, reorg/reconsideration, and CSV export.
`TestRealAsyncSwapRecoveryAndBounties` also checks linked funding, owner claim/refund,
and tower-earned activity against actual settlement output amounts. Run serially
against the isolated fixture, then repeat with `BLAKESWAP_TEST_ELECTRUM=1`:

```sh
BLAKESWAP_REGTEST="$PWD" sh scripts/go.sh test -p=1 ./internal/api ./internal/daemon -run 'TestReal(ActivityHistoryThroughTypedAPI|AsyncSwapRecoveryAndBounties)' -v -count=1
```

`ActivityTests` covers native loading/empty/error/retry states, frozen load-more,
details/navigation IDs, discarded late replies after wallet/network/filter changes,
whole-scope CSV chunks, and explicit chain/network explorer binding. These are
production-model/view compilation tests, not pixel-level UI automation. Build a
fresh packaged helper before running the complete native suite as described above.

## Market discovery and durable management

`TestMarket*` checks arbitrary-precision rate comparisons above int64 product
bounds, rates with the same rounded display, both directions and independent
owner/amount filters, deterministic ties, revision-checked pages, expiry bounds,
stale/pending availability, and own history after the public book disappears.
`TestOrderReplacement*` and `TestOrderCancellation*` race maker acceptance,
validate exact source/wallet changes, reject insufficient new funds, check a
single exclusive reservation, and reopen an actual vault interrupted between
pending authorization and the atomic replacement save. Identical retries survive
quote expiry and changed request revisions fail. Terminal refunded orders retain
signed terms and can only be recreated with a fresh quote.

`TestMarketTypedFieldsAndOrderReviewBinding` covers protobuf conversion of exact
amounts, custom expiry, publication/lineage, and review/cancellation bindings.
The desktop advisory lifecycle regression includes `market.list`: it cannot hold
the global lifecycle lock or outlive engine closure. Native `MarketTests` covers
all displayed statuses, publication labels, empty filtered results, paging, stale
wallet/network/generation/filter responses, cancellation binding, management
review fields, and order-to-swap destinations.

`TestRealManagedOrderThroughTypedAPI` requires actual BTC and Blake regtest nodes.
For each sell direction it creates an offer with custom expiry, distinguishes
local commit from positive relay ACK, replaces it, submits a deliberately stale
signed request, verifies rejection before funding, then settles the accepted new
version and reopens its linked history/confirmation receipt. Run RPC and Electrum
separately, with the shared fixture locked and real-node packages serialized:

```sh
BLAKESWAP_REGTEST=/absolute/path/to/task-fixture \
BLAKESWAP_BTC_RPC_PORT=39443 BLAKESWAP_BLAKE_RPC_PORT=49443 \
  sh scripts/go.sh test -race -count=1 -p 1 ./internal/api -run '^TestRealManagedOrderThroughTypedAPI$' -v
# Repeat with BLAKESWAP_TEST_ELECTRUM=1 against the same isolated native nodes.
```

Ordinary tests without `BLAKESWAP_REGTEST` skip this matrix and are not integration
evidence. No public offers, wallets, funds, or relay writes are needed.

### Portable restore safety

Portable storage/desktop tests cover chosen passwords, authenticated manifests, interrupted export/import, unchanged source files on failure, all-network receive/recovery state, duplicate identities including interrupted profiles, legacy input, re-exported recovery holds and typed post-onboarding API import. A large-history regression resumes an interrupted portable installation above the legacy 64 MiB database limit, preserves its source bytes and recovery gate, and rejects oversized installed vaults before copying. Native helper tests export after setup, inspect and import into an existing unrelated profile, reject duplicates and expose recovery-in-progress while local endpoints are unavailable.

`TestRestored*` exercises both owner roles: private prepared claims/funding stay held even when both chains respond; observed secrets survive reload and peer outage; restored refunds need positive incoming refunds; permanent incoming-claim knowledge survives later conflicting/reorg observations. Positive confirmed outcomes permit readiness, reorgs remove it, and empty known-obligation snapshots or final never-funded cancellations can become ready after full synchronization. Skipped cached-confirmed payments remain held; single-leg confirmed refunds and durable nonfunding plus peer refund can resolve without weakening active-snapshot holds. Positive variant evidence survives bounded slices only while canonical history/source stay current and is discarded on restart or reorg. A positively confirmed original or replacement completes its payment proof without waiting for unavailable conflicting variants. Restored completed claims retry after missing or depth-lost current outcomes, while unavailable/incomplete scans preserve terminal history. Confirmed restored tower refunds update their outcome and bounty history without permitting a new refund publication; fresh reorgs reopen the hold.

`TestRestored*NetworkGuardAfterKnownReorg` covers maker/taker completed/refunded owners, both tower target assets, and confirmed payments: a positive checkpoint contradiction holds both live and stored network changes before incomplete scans converge. Rewind, competing ancestor, mixed-tip, immediate durable writes, failed writes, vault reload, re-export, backup freshness, positive clearance, and ordinary outage/source-change controls are covered. Tower target settlement clears independently during a peer outage; final never-funded local decisions stay final.

`TestOrderRecovery*` verifies validated quarantined order backfill, unknown timestamps/publication, manual recreation only after recovery is ready, exact source identity and signature checks, fresh funds/checkpoint checks, new IDs, retained lineage, receipt idempotency after restart, and no old queued publication or cancellation/replacement authority.

With the isolated real-node fixture, run `TestRealPortableRestoreBeforeFundingPublication`, `TestRealPortableRestoreWitnessAndReorg` , `TestRealPortableRestorePreservesRefunds`, `TestRealPortableTowerRecovery`, `TestRealPortablePaymentVariantsAndReceiveIndexes`, and `TestRealPortableTowerRefundObservesConfirmedOutcome` through RPC and the Electrum fixture. They use the portable encrypted manifest shape and chosen password to reload daemon state against newer chain state. Private publication crash snapshots must not create funding or expose a secret. Actual witnessed claims recover through alternating chain outages, confirm with preserved authorizations, and return to recovery after an invalidated claim block before retrying their existing signed claim and reconfirming. The restored tower refund case observes an already-authorized later publication, records its exact bounty without a new broadcast, and immediately holds monitoring after an actual reorg while bounded target scans reconcile the prior display. Actual owner refunds retain their signed ladder and become ready only after a positively observed incoming refund and both confirmed outcomes. Desktop/native tests separately cover the installer/profile boundary; real-node flags unset means these cases are skipped, not an integration pass.

The portable tower case deliberately starts with cold observation and target RPC cursors. Each first 200ms slice must retain validated historical progress while withholding publication; subsequent paced cycles must learn the positive public witness and then recover the target claim within 12 cycles and a 30-second deadline per phase. Archive/restart and witness-block invalidation occur between those phases. Per-tick production budgets, target-outage holds, witness retention, exact bounty, refund suppression and confirmation assertions remain unchanged.

`TestRestoredActivity*` and `TestRestoredLegacyOrders*` verify preserved audit
origin with the current API/CSV profile, stale observation invalidation, retained
prior outcomes, unknown-time legacy cancelled/open orders and no republication.
Portable manifest/import tests round-trip activity, receipt and per-network
history state. Fingerprint tests distinguish polling/progress changes from new
replacement variants, receipt facts and reorg outcomes.

## Automatic offer policies

`TestAutomation*` covers exact rate/rounding bounds, chosen-maker provenance and
freshness, quorum/spread failures, typed full authorization, fixed renewal,
concurrent ticks, durable receipt/successor/charge transitions, restart without
catch-up, immutable accepted sources, manual disable, unavailable fees/provider,
network changes and reserved/committed budget preservation. Native
`AutomationTests` exercises exact fields, full-review save binding, discarded
stale-context responses, disable choices and restored-budget display.

`TestRealAutomaticOfferPolicyUpdatePreservesAcceptedTrade` uses generated gRPC,
private relay and isolated wallets on actual BTC/Blake regtest nodes. It creates
an automatic offer in each sell direction, accepts it, updates the policy while
the daemon advances settlement, confirms original principals and exact 6,500-sat
funding fees from both nodes, and reopens the policy to check immutable committed
charges and disabled state. Run under the exclusive fixture lock, then repeat
with `BLAKESWAP_TEST_ELECTRUM=1`:

```sh
BLAKESWAP_REGTEST=/absolute/path/to/isolated-fixture \
BLAKESWAP_BTC_RPC_PORT=39443 BLAKESWAP_BLAKE_RPC_PORT=49443 \
  sh scripts/go.sh test -race -p=1 -count=1 ./internal/api \
  -run '^TestRealAutomaticOfferPolicyUpdatePreservesAcceptedTrade$' -v
```

Ordinary unit runs skip this real-node scenario and are not integration evidence.
No public funds, user wallets or public offer publication are needed.

`TestAutomationImported*`, `TestAutomationDisabledAcknowledgement*`, and
`TestAutomationHeldOffer*` cover pre-install policy holds, pending-grant retirement,
accepted receipt preservation, stale signed-order rejection, current recovery
requirements, explicit profile-bound resumption, permanent imported reservation
accounting and re-import. `TestImportedAutomationNeverResumesOldSpendingAuthority`
uses actual encrypted legacy and portable first-wallet installation paths.
`TestAutomationBackupPreservesAuthorizationAndUncertainty` verifies deep-copy
and backup fingerprint coverage for uncertainty and successor identity.

`TestAutomationInvalidDurableState*` verifies malformed policy/charge pointers,
identities and accounting are rejected before daemon load or recovery, without
changing durable state or leaking the vault lock. `TestAutomationMalformedBackup*`
checks authenticated legacy/portable import rejects those records before profile
publication and preserves existing settings, wallets and source files. Backup
fingerprint tests retain all archive fields while distinguishing no-op cadence
checks from configuration, holds, receipts, charges and real actions.

## Deadline alerts and shutdown protection

`TestAction*` covers deterministic height/MTP cutoffs and strict timestamp
finality, peer-clock skew/outage, stale observations, asymmetric reveal safety
margins, armed-tower first-revelation distinction, restored reorg holds, signed
sends, enabled automation, known-empty offline state and missing local knowledge.
Desktop cases query all saved wallets independently of the selected page and
exercise in-flight command/tick boundaries plus the bounded nonblocking refresh
queue. The typed API test authenticates the all-wallet response and preserves
exact timestamps/revisions and uncertainty through gRPC.

`MonitoringTests` injects clocks and notification delivery for durable private
journal permissions, restart/dedup, preferences/denial, initial terminal history,
reorg reopening and wake reconciliation across healthy/unavailable wallets.
Suspended-provider cases verify interruption, newer summaries, changed
preferences and expired observations stop obsolete batches. The default
notification provider is also exercised outside a native app bundle.
`DaemonProcessTests` uses `BLAKESWAP_TEST_HELPER` from the fresh signed build to
exercise Stay open then explicit Quit through the last-window handler, normal
empty shutdown, unknown-state explicit shutdown and private runtime cleanup. A
separate owned-child fixture refuses SIGTERM to verify the bounded fallback.
Two actual helpers sharing one isolated root verify a rejected second owner
cannot accept or remove the first runtime. A suspended actual child verifies
forced cleanup using its PID and launch nonce.
These tests never create an always-on service or use public wallets.

## Inventory strategies

`TestStrategy*` exercises exact two-sided sizing/prices, shared per-chain caps,
manual whole-input reservations, unconfirmed/HTLC exclusions, selected funding
fees, imported uncertainty/exposure, duplicate/restart/breaker behavior, recovery
scanner proofs and reorg holds. Report controls cover exact owned swap/transaction
attribution, a foreign maker reusing the same order ID, current confirmed fees,
refunds, cancellation and immutable gross consumption. Native `StrategyTests`
uses injected typed responses to check full review/context binding, stop/import
state, and production view compilation.

`TestRealInventoryStrategyTradeAndRefund` uses a private relay and isolated wallet
profiles on real BTC/BLAKE regtest nodes. It completes each maker direction and a
stopped strategy refund, verifies immutable principals and exact 6,500-sat funding
fees against raw node transactions, and reconciles confirmed inventory and T08
activity fees independently from conservative lifetime charges after restart.
Run serially under the exclusive fixture lock, then repeat with
`BLAKESWAP_TEST_ELECTRUM=1`:

```sh
BLAKESWAP_REGTEST=/absolute/path/to/isolated-fixture \
BLAKESWAP_BTC_RPC_PORT=39443 BLAKESWAP_BLAKE_RPC_PORT=49443 \
  sh scripts/go.sh test -race -p=1 -count=1 ./internal/api \
  -run '^TestRealInventoryStrategyTradeAndRefund$' -v
```

A run without `BLAKESWAP_REGTEST` compiles and skips these scenarios. It does not
establish actual-chain settlement, inventory or fee correctness.

## T09 archive, synchronization and resource acceptance

Run focused archive/capacity/mailbox tests with `BLAKESWAP_REGTEST= sh scripts/go.sh test ./internal/storage ./internal/daemon ./internal/desktop -run 'Archive|Capacity|Mailbox|Portable|Stream' -count=1`. Storage tests cover encrypted identity binding, atomic ownership, duplicate/collision rejection, process termination before/after commit, framing order/completeness and private import staging. Restored reactivation tests fill the entire 64-core batch and verify that original authority markers and exact fee companions precede execution; malformed companion reads cannot expose a core record.

`TestCapacityPaymentContinuationEncoding` signs 50-input payments on both chains and retains all 16 variants plus the current copy. `TestCapacitySwapContinuationEncoding` retains both 50-input funding legs, the three owner claim/refund variants, two three-template tower jobs, receipts and outbound encrypted messages. These are measured normal-operation shapes with explicit signature-length headroom, not a universal bound on arbitrary previously accepted imports. Metadata-only capacity tests deliberately inject archive stats to prove active/disk accounting without pretending to be physical workloads.

The opt-in physical test uses real private files and no chain nodes:

```sh
/usr/bin/time -l env BLAKESWAP_REGTEST= BLAKESWAP_PORTABLE_SCALE=1 BLAKESWAP_SCALE_RECORDS=95000 \
  sh scripts/go.sh test ./internal/desktop \
  -run '^TestPortablePhysicalLargeHistoryAndCoreContinuation$' -count=1 -timeout=20m -v
```

It writes and imports an accepted near-limit v1 population, creates a physical encrypted cold archive, adds retained core payloads so the complete output exceeds the old v1 envelope, and exports/installs a complete v2 profile. The phase log separates original v1 parsing, cold storage, total source preparation/validation, the separate **complete worker join/save/capture/resume pause**, export, and v2 validation/restore. The 20ms sampled Go heap high-water marks are measurements, not strict peak guarantees; `/usr/bin/time -l` records OS peak RSS. Payload growth in this format test is not a claim of actual broadcast protocol validity. Run it without concurrent broad native/Go builds or node integration to make memory and latency results interpretable. A smaller `BLAKESWAP_SCALE_RECORDS=1000` run checks fixture mechanics but does not satisfy the large physical workload.

The snapshot capture regressions force the non-clone fallback even on APFS. They
exercise concurrent 8 MiB bbolt writer growth from an archive callback, exact
retry and delete/reinsert generation fences, final-callback mutation, cancellation,
independent cloned credentials, private cleanup and late semantic freshness.
`TestPortableCaptureWalletWorkerProgressDuringPreparationAndMaterialization`
runs the real daemon and wallet worker against disposable HTTP observation
fixtures and proves chain reads continue during inactive credential preparation
and a deliberately blocked archive callback. This is scheduling evidence, not a
real-chain settlement test. The physical fixture reports `source_snapshot_total`
separately from the complete `source_snapshot_worker_pause`, including worker
join/save/capture/resume; its large-history measurement remains the latency test.

At checkpoint `da8858609f2fd8c30a733045dfcf9557ba54e9b6` on the measured APFS
host, 95,000 retained activity rows and 2,021 core obligations produced a
232,874,099-byte accepted v1 input and a 284,706,570-byte complete v2 output.
The full export/restore fixture passed in 120.251 seconds. Source preparation and
private validation took 10.359 seconds in total; the common worker pause was
79.627 milliseconds, compared with 31.280 seconds before capture was separated.
A small independent real-daemon/HTTP fixture verifies worker progress outside
that interval. These are measurements on this host, not universal timing bounds.

Maximum process RSS was 2,179,645,440 bytes; peak sampled heap was 1,436,303,512
bytes during the unchanged legacy v1 import. Sampled summed file allocation peaked at
1,829,212,160 bytes (logical size 1,863,316,815), with hardlinked inodes counted
once. Separate APFS clone inodes can share extents; clone sharing is not
deduplicated, so this is not a unique physical-disk high-water measurement.
Disk samples run every 200ms and heap samples every 20ms, so brief higher
peaks may be missed. Host-wide swap-in counters advanced by eight 16KiB pages;
swap-out counters were unchanged. Active checkpoint/core and individual-record
memory costs remain, even though cold history does not require a full graph.


The history query regressions exercise selected-kind encrypted collection rather
than whole-wallet reconstruction. `TestHistoryRowsFreezeAcrossColdMutationAndAgreeWithCSV`
checks 401 mixed hot/cold rows, timestamp ties, stable pagination and CSV agreement,
then later mutation, FIFO eviction and close cleanup. The cold-market equivalence
matrix checks 72 owner/status/sort/direction combinations and exact revisions against
the existing live projection, including rational rates whose products exceed int64.
Queued cancellation and late-context failure tests verify result key/file disposal.
Storage tests cover 17,003-row multi-level encrypted merging, malformed ciphertext,
truncation, ordinal/cross-result substitution and final-callback archive ABA.

`TestHistoryVisitorDoesNotPinSettlementWriter` requires a real 8 MiB save to finish
before a blocked visitor callback is released, with a five-second bound and joined
read/write goroutines on every outcome. Merely observing a runnable bbolt mmap
frame is not a lock failure: normal file growth dereferences pages after acquiring
the mmap lock. The behavioral test still fails against the original pinned reader.

Cold strategy report tests preserve hour-old observation times while requiring
the row's own current canonical prefix. Outage, unrelated/repaired prefixes,
source changes, absent proof and held ownership remain incomplete. The realistic
import regression follows signed maker completion, core/history compaction,
private streamed promotion/quarantine/import, restored positive scanning and
rearchive while the strategy stays disabled under RestoreHold. Its reorg variant
starts with imported live anchors cleared, proves the retained history prefix
contradiction, checks the new-work hold and bounded promotion, then verifies a new
row binding, complete report and retention of the old confirmed outcome.

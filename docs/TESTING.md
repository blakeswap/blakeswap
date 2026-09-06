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

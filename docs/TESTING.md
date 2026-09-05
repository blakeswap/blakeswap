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

import XCTest
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class TradeReviewTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "wallet-key")
    private func root() throws -> String {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        addTeardownBlock { try? FileManager.default.removeItem(at: url) }
        return url.path
    }
    private func quote(kind: String = "maker") -> Blakeswap_V1_TradeQuote {
        var q = Blakeswap_V1_TradeQuote()
        q.wallet = context.profile; q.walletKey = context.walletKey; q.network = context.network; q.kind = kind
        q.token = String(repeating: "a", count: 64); q.revision = String(repeating: "b", count: 64)
        q.ready = true; q.expires = Int64(Date().timeIntervalSince1970) + 120
        q.paidChain = kind == "maker" ? "btc" : "blake"; q.receivedChain = kind == "maker" ? "blake" : "btc"
        q.paidPrincipal = 100_000; q.paidTotal = 102_000; q.receivedPrincipal = 200_000
        return q
    }
    func testReviewAndCancelHaveNoConfirmationOrJournalMutation() async throws {
        for kind in ["maker", "taker"] {
            let directory = try root(), q = quote(kind: kind)
            var calls: [String] = []
            let model = TradeReviewModel(context: context, root: directory) { method, data in
                calls.append(method)
                let request = try Blakeswap_V1_TradeQuoteRequest(jsonUTF8Data: data)
                XCTAssertEqual(request.expectedWallet, self.context.profile)
                XCTAssertEqual(request.expectedNetwork, self.context.network)
                return try q.serializedData()
            }
            var draft = Blakeswap_V1_TradeQuoteRequest(); draft.kind = kind
            await model.review(draft, current: { self.context })
            XCTAssertEqual(model.quote?.paidChain, q.paidChain)
            XCTAssertEqual(model.quote?.receivedChain, q.receivedChain)
            model.back()
            XCTAssertNil(model.quote); XCTAssertEqual(calls, ["trade.quote"])
            XCTAssertNil(try TradeConfirmationJournal(root: directory).load(profile: context.profile, network: context.network))
        }
    }
    func testDelayedQuoteCannotApplyAcrossWalletNetworkOrGenerationChanges() async throws {
        for replacement in [TradeContext(profile: "bob", network: "regtest", generation: 2, walletKey: "other"), TradeContext(profile: "alice", network: "mainnet", generation: 2, walletKey: "wallet-key"), TradeContext(profile: "alice", network: "regtest", generation: 3, walletKey: "wallet-key")] {
            var current = context
            let q = quote()
            let model = TradeReviewModel(context: context, root: try root()) { _, _ in current = replacement; return try q.serializedData() }
            var draft = Blakeswap_V1_TradeQuoteRequest(); draft.kind = "maker"
            await model.review(draft, current: { current })
            XCTAssertNil(model.quote); XCTAssertNil(model.error)
        }
    }
    func testDoubleClickAndAmbiguousRetryKeepOneDurableIdentityAfterRestartAndExpiry() async throws {
        let directory = try root(), q = quote(kind: "taker")
        var calls = 0
        var original: Blakeswap_V1_ConfirmTradeRequest?
        var resume: CheckedContinuation<Data, Error>?
        let model = TradeReviewModel(context: context, root: directory) { _, data in
            calls += 1; original = try Blakeswap_V1_ConfirmTradeRequest(jsonUTF8Data: data)
            XCTAssertNotNil(try TradeConfirmationJournal(root: directory).load(profile: self.context.profile, network: self.context.network))
            return try await withCheckedThrowingContinuation { resume = $0 }
        }
        model.quote = q
        let first = Task { await model.confirm(current: { self.context }) }
        while resume == nil { await Task.yield() }
        await model.confirm(current: { self.context })
        XCTAssertEqual(calls, 1)
        resume?.resume(throwing: RPCError.message("Response lost")); await first.value
        XCTAssertNotNil(model.pending); XCTAssertNil(model.acceptedID)
        var expired = q; expired.expires = 1
        let restored = TradeReviewModel(context: context, root: directory) { method, data in
            XCTAssertEqual(method, "trade.confirm")
            let request = try Blakeswap_V1_ConfirmTradeRequest(jsonUTF8Data: data)
            XCTAssertEqual(request, original)
            var result = Blakeswap_V1_ConfirmTradeResult(); result.id = request.requestID; result.kind = "taker"; result.state = "accepted"
            return try result.serializedData()
        }
        restored.quote = expired
        await restored.confirm(current: { self.context })
        XCTAssertEqual(restored.acceptedID, original?.requestID); XCTAssertEqual(restored.acceptedKind, "taker")
        await restored.confirm(current: { self.context }) // A queued second click after success cannot create another identity.
        XCTAssertNil(restored.pending)
        XCTAssertNil(try TradeConfirmationJournal(root: directory).load(profile: context.profile, network: context.network))
    }
    func testDelayedConfirmationDoesNotApplyToAnotherWalletButResolvesOriginalJournal() async throws {
        let directory = try root()
        var current = context
        let model = TradeReviewModel(context: context, root: directory) { _, data in
            let request = try Blakeswap_V1_ConfirmTradeRequest(jsonUTF8Data: data)
            current = TradeContext(profile: "bob", network: "regtest", generation: 2, walletKey: "other")
            var result = Blakeswap_V1_ConfirmTradeResult(); result.id = request.requestID; result.kind = "maker"; result.state = "accepted"
            return try result.serializedData()
        }
        model.quote = quote()
        await model.confirm(current: { current })
        XCTAssertNil(model.acceptedID); XCTAssertNil(model.error)
        XCTAssertNil(try TradeConfirmationJournal(root: directory).load(profile: context.profile, network: context.network))
    }
    func testDefinitiveRejectionClearsIdentityAndRequiresFreshReview() async throws {
        let directory = try root()
        let model = TradeReviewModel(context: context, root: directory) { _, data in
            let request = try Blakeswap_V1_ConfirmTradeRequest(jsonUTF8Data: data)
            var result = Blakeswap_V1_ConfirmTradeResult(); result.id = request.requestID; result.kind = "maker"; result.state = "rejected"; result.error = "Provider changed"
            return try result.serializedData()
        }
        model.quote = quote(); await model.confirm(current: { self.context })
        XCTAssertNil(model.quote); XCTAssertNil(model.pending); XCTAssertEqual(model.error, "Provider changed")
        XCTAssertNil(try TradeConfirmationJournal(root: directory).load(profile: context.profile, network: context.network))
    }
    func testJournalIsPrivateMinimalAndCannotOverwriteAnUnresolvedConfirmation() throws {
        let directory = try root(), journal = TradeConfirmationJournal(root: directory)
        let first = PendingTradeConfirmation(profile: context.profile, network: context.network, requestID: String(repeating: "c", count: 64), token: String(repeating: "a", count: 64), revision: String(repeating: "b", count: 64), kind: "maker")
        try journal.save(first)
        XCTAssertEqual(try journal.load(profile: context.profile, network: context.network), first)
        let files = try FileManager.default.contentsOfDirectory(at: URL(fileURLWithPath: directory).appendingPathComponent("pending-trades"), includingPropertiesForKeys: nil)
        XCTAssertEqual(files.count, 1)
        let attrs = try FileManager.default.attributesOfItem(atPath: files[0].path)
        XCTAssertEqual((attrs[.posixPermissions] as? NSNumber)?.intValue, 0o600)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(contentsOf: files[0])) as? [String: Any])
        XCTAssertEqual(Set(object.keys), Set(["profile", "network", "requestID", "token", "revision", "kind"]))
        let other = PendingTradeConfirmation(profile: context.profile, network: context.network, requestID: String(repeating: "d", count: 64), token: first.token, revision: first.revision, kind: first.kind)
        XCTAssertThrowsError(try journal.save(other)); XCTAssertThrowsError(try journal.clear(other))
        XCTAssertEqual(try journal.load(profile: context.profile, network: context.network), first)
    }
}

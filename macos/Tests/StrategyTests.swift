import XCTest
import SwiftUI
import AppKit
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class StrategyTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "key")
    func testExactTwoAssetDraftAndFullAuthorizationBinding() async throws {
        var draft = StrategyDraft(); draft.btc.maximum = "9999999999"; draft.btc.volume = "2100000000000000"
        draft.common.rateN = "2100000000000000"; draft.common.rateD = "2099999999999999"
        draft.common.reference = "orderbook"; draft.common.makers = "one\ntwo\nthree"; draft.common.enabled = true
        var calls: [String] = []
        let model = StrategyModel(context: context, root: "/unused") { method, data in
            calls.append(method)
            let p = try Blakeswap_V1_StrategyEdit(jsonUTF8Data: data)
            XCTAssertEqual(p.config.wallet, self.context.profile); XCTAssertEqual(p.config.network, self.context.network)
            XCTAssertEqual(p.config.btc.maxOffer, 9_999_999_999); XCTAssertEqual(p.config.btc.volumeLimit, 2_100_000_000_000_000)
            XCTAssertEqual(p.config.rate.denominator, 2_099_999_999_999_999); XCTAssertEqual(p.config.referenceMakers.count, 3)
            if method == "strategy.review" {
                var r = Blakeswap_V1_StrategyReview(); r.config = p.config; r.enabled = p.enabled; r.expectedRevision = p.expectedRevision; r.reviewDigest = "exact"
                return try r.serializedData()
            }
            XCTAssertEqual(p.reviewDigest, "exact")
            var v = Blakeswap_V1_StrategyView(); v.config = p.config; v.enabled = p.enabled; v.revision = p.expectedRevision + 1
            return try v.serializedData()
        }
        let beforeReview = await model.save(current: { self.context }); XCTAssertFalse(beforeReview)
        await model.review(draft, current: { self.context }); XCTAssertNotNil(model.review)
        let saved = await model.save(current: { self.context }); XCTAssertTrue(saved)
        XCTAssertEqual(calls, ["strategy.review", "strategy.save"])
        for invalid in ["1.25", "-1", "9223372036854775808"] {
            draft.btc.volume = invalid; XCTAssertThrowsError(try draft.edit(context: context))
        }
        let app = AppModel()
        XCTAssertNotNil(NSHostingView(rootView: StrategyView(context: context, root: "/unused").environmentObject(app)).rootView)
        XCTAssertNotNil(NSHostingView(rootView: StrategyEditor(context: context, root: "/unused", draft: StrategyDraft()).environmentObject(app)).rootView)
    }
    func testLateStrategyReviewCannotAuthorizeAnotherWalletOrGeneration() async throws {
        var current = context
        var resume: CheckedContinuation<Data, Error>?
        var request: Blakeswap_V1_StrategyEdit?
        let model = StrategyModel(context: context, root: "/unused") { _, data in
            request = try Blakeswap_V1_StrategyEdit(jsonUTF8Data: data)
            return try await withCheckedThrowingContinuation { resume = $0 }
        }
        let task = Task { await model.review(StrategyDraft(), current: { current }) }
        while resume == nil { await Task.yield() }
        current = TradeContext(profile: "alice", network: "regtest", generation: 2, walletKey: "key")
        var result = Blakeswap_V1_StrategyReview(); result.config = request!.config; result.reviewDigest = "old"
        resume?.resume(returning: try result.serializedData()); await task.value
        XCTAssertNil(model.review)
        let saved = await model.save(current: { current }); XCTAssertFalse(saved)
    }
    func testStopBindingsAndVisibleRestoredBudget() async throws {
        var policy = Blakeswap_V1_StrategyView(); policy.config.id = "id"; policy.config.wallet = context.profile; policy.config.network = context.network
        policy.revision = 8; policy.restoreHold = true; policy.inventory["btc", default: Blakeswap_V1_StrategyInventory()].committedVolume = 1000000
        var disabled = false
        let model = StrategyModel(context: context, root: "/unused") { method, data in
            if method == "strategy.stop" {
                let p = try Blakeswap_V1_StopStrategyRequest(jsonUTF8Data: data)
                XCTAssertEqual(p.expectedRevision, 8); XCTAssertEqual(p.expectedWallet, self.context.profile); XCTAssertEqual(p.expectedNetwork, self.context.network); XCTAssertTrue(p.stop)
                disabled = true; return Data()
            }
            var page = Blakeswap_V1_StrategyList(); page.wallet = self.context.profile; page.network = self.context.network; page.strategies = [policy]
            return try page.serializedData()
        }
        await model.stop(policy, permanent: true, current: { self.context })
        XCTAssertTrue(disabled); XCTAssertEqual(model.strategies.first?.inventory["btc"]?.committedVolume, 1000000)
        let draft = StrategyDraft(policy); XCTAssertTrue(draft.common.restored); XCTAssertFalse(draft.common.acknowledgeRestored)
    }
}

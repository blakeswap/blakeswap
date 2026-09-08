import XCTest
import SwiftUI
import AppKit
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class AutomationTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "key")
    func testExactPolicyDraftAndFullAuthorizationBinding() async throws {
        var draft = AutomationDraft(); draft.size = "9999999999"; draft.volume = "2100000000000000"
        draft.rateN = "2100000000000000"; draft.rateD = "2099999999999999"
        draft.reference = "orderbook"; draft.makers = "one\ntwo\nthree"; draft.enabled = true
        var calls: [String] = []
        let model = AutomationModel(context: context, root: "/unused") { method, data in
            calls.append(method)
            let p = try Blakeswap_V2_AutomationEdit(jsonUTF8Data: data)
            XCTAssertEqual(p.config.wallet, self.context.profile); XCTAssertEqual(p.config.network, self.context.network)
            XCTAssertEqual(p.config.sellAmount, 9_999_999_999); XCTAssertEqual(p.config.volumeLimit, 2_100_000_000_000_000)
            XCTAssertEqual(p.config.rate.denominator, 2_099_999_999_999_999); XCTAssertEqual(p.config.referenceMakers.count, 3)
            if method == "automation.review" {
                var r = Blakeswap_V2_AutomationReview(); r.config = p.config; r.enabled = p.enabled; r.expectedRevision = p.expectedRevision; r.reviewDigest = "exact"
                return try r.serializedData()
            }
            XCTAssertEqual(p.reviewDigest, "exact")
            var v = Blakeswap_V2_AutomationView(); v.config = p.config; v.enabled = p.enabled; v.revision = p.expectedRevision + 1
            return try v.serializedData()
        }
        let beforeReview = await model.save(current: { self.context }); XCTAssertFalse(beforeReview)
        await model.review(draft, current: { self.context }); XCTAssertNotNil(model.review)
        let saved = await model.save(current: { self.context }); XCTAssertTrue(saved)
        XCTAssertEqual(calls, ["automation.review", "automation.save"])
        for invalid in ["1.25", "-1", "9223372036854775808"] {
            draft.volume = invalid; XCTAssertThrowsError(try draft.edit(context: context))
        }
        let app = AppModel()
        XCTAssertNotNil(NSHostingView(rootView: AutomationView(context: context, root: "/unused").environmentObject(app)).rootView)
        XCTAssertNotNil(NSHostingView(rootView: AutomationEditor(context: context, root: "/unused", draft: AutomationDraft()).environmentObject(app)).rootView)
    }
    func testLatePolicyReviewCannotAuthorizeAnotherWalletOrGeneration() async throws {
        var current = context
        var resume: CheckedContinuation<Data, Error>?
        var request: Blakeswap_V2_AutomationEdit?
        let model = AutomationModel(context: context, root: "/unused") { _, data in
            request = try Blakeswap_V2_AutomationEdit(jsonUTF8Data: data)
            return try await withCheckedThrowingContinuation { resume = $0 }
        }
        let task = Task { await model.review(AutomationDraft(), current: { current }) }
        while resume == nil { await Task.yield() }
        current = TradeContext(profile: "alice", network: "regtest", generation: 2, walletKey: "key")
        var result = Blakeswap_V2_AutomationReview(); result.config = request!.config; result.reviewDigest = "old"
        resume?.resume(returning: try result.serializedData()); await task.value
        XCTAssertNil(model.review)
        let saved = await model.save(current: { current }); XCTAssertFalse(saved)
    }
    func testDisableBindingsAndVisibleRestoredBudget() async throws {
        var policy = Blakeswap_V2_AutomationView(); policy.config.id = "id"; policy.config.wallet = context.profile; policy.config.network = context.network
        policy.revision = 8; policy.restoreHold = true; policy.usage.committedVolume = 1000000
        var disabled = false
        let model = AutomationModel(context: context, root: "/unused") { method, data in
            if method == "automation.disable" {
                let p = try Blakeswap_V2_DisableAutomationRequest(jsonUTF8Data: data)
                XCTAssertEqual(p.expectedRevision, 8); XCTAssertEqual(p.expectedWallet, self.context.profile); XCTAssertEqual(p.expectedNetwork, self.context.network); XCTAssertTrue(p.cancelOpen)
                disabled = true; return Data()
            }
            var page = Blakeswap_V2_AutomationList(); page.wallet = self.context.profile; page.network = self.context.network; page.policies = [policy]
            return try page.serializedData()
        }
        await model.disable(policy, cancelOpen: true, current: { self.context })
        XCTAssertTrue(disabled); XCTAssertEqual(model.policies.first?.usage.committedVolume, 1000000)
        let draft = AutomationDraft(policy); XCTAssertTrue(draft.restored); XCTAssertFalse(draft.acknowledgeRestored)
    }
}

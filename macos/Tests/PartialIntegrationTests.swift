import Foundation
import SwiftProtobuf
import XCTest
@testable import Blakeswap

@MainActor
final class PartialIntegrationTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "owned-maker")
    private func fixture() throws -> (String, PendingTradeConfirmation) {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        addTeardownBlock { try? FileManager.default.removeItem(at: root) }
        let saved = PendingTradeConfirmation(profile: context.profile, network: context.network, requestID: String(repeating: "a", count: 64), token: String(repeating: "b", count: 64), revision: String(repeating: "c", count: 64), kind: "taker")
        try TradeConfirmationJournal(root: root.path).save(saved)
        return (root.path, saved)
    }
    private func result(_ saved: PendingTradeConfirmation, state: String = "accepted") throws -> Data {
        var reply = Blakeswap_V1_ConfirmTradeResult()
        reply.id = saved.requestID; reply.kind = saved.kind; reply.state = state
        return try reply.serializedData()
    }
    func testSavedFinalOutcomeHasNoNewConsentAndClearsOnlyOriginalJournal() async throws {
        let (root, saved) = try fixture(); var current = context
        var reads = 0, authorizations = 0
        let model = TradeReviewModel(context: context, root: root, readConfirmation: { request in
            reads += 1; XCTAssertEqual(request, saved)
            current = TradeContext(profile: "bob", network: "mainnet", generation: 3, walletKey: "other")
            return try self.result(saved)
        }, call: { _, _ in authorizations += 1; throw NativeSecurityError.denied })
        await model.confirm(current: { current })
        XCTAssertEqual(reads, 1); XCTAssertEqual(authorizations, 0)
        XCTAssertNil(model.acceptedID); XCTAssertNil(model.pending)
        XCTAssertNil(try TradeConfirmationJournal(root: root).load(profile: context.profile, network: context.network))
    }
    func testSavedUnknownOutcomeKeepsIdentityUntilExplicitSameRequestAuthorization() async throws {
        for failure in [NativeSecurityError.denied, .unavailable, .closed] {
            let (root, saved) = try fixture(); var calls = 0, reads = 0
            let model = TradeReviewModel(context: context, root: root, readConfirmation: { request in
                reads += 1; XCTAssertEqual(request, saved); throw failure
            }, call: { method, raw in
                calls += 1; XCTAssertEqual(method, "trade.confirm")
                XCTAssertEqual(try Blakeswap_V1_ConfirmTradeRequest(jsonUTF8Data: raw), saved.request)
                return try self.result(saved)
            })
            await model.confirm(current: { self.context })
            XCTAssertEqual(calls, 0); XCTAssertEqual(reads, 1); XCTAssertEqual(model.pending, saved)
            XCTAssertEqual(try TradeConfirmationJournal(root: root).load(profile: context.profile, network: context.network), saved)
            await model.confirm(authorizeSaved: true, current: { self.context })
            XCTAssertEqual(calls, 1); XCTAssertEqual(reads, 1); XCTAssertEqual(model.acceptedID, saved.requestID)
        }
    }
    func testPendingAndChangedReceiptNeverCreateAnotherQuoteOrClearJournal() async throws {
        for state in ["pending", "wrong-identity"] {
            let (root, saved) = try fixture(); var calls = 0
            let model = TradeReviewModel(context: context, root: root, readConfirmation: { _ in
                var reply = Blakeswap_V1_ConfirmTradeResult(); reply.id = state == "pending" ? saved.requestID : "foreign"
                reply.kind = saved.kind; reply.state = state == "pending" ? "pending" : "accepted"
                return try reply.serializedData()
            }, call: { _, _ in calls += 1; throw NativeSecurityError.denied })
            await model.confirm(current: { self.context })
            XCTAssertEqual(calls, 0); XCTAssertEqual(model.pending, saved); XCTAssertNil(model.acceptedID)
            XCTAssertEqual(try TradeConfirmationJournal(root: root).load(profile: context.profile, network: context.network), saved)
        }
    }
    func testReceiptLookupRefusesFileModeBeforeAnyTransport() async throws {
        let (root, saved) = try fixture()
        let runtime: [String: Any] = ["alice": ["socket": "/must-not-connect", "http": "", "token": String(repeating: "a", count: 64), "credential_mode": "file"]]
        let path = URL(fileURLWithPath: root).appendingPathComponent("runtime.json")
        try JSONSerialization.data(withJSONObject: runtime).write(to: path)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path.path)
        do { _ = try await DaemonRPC.readConfirmation(root: root, saved: saved); XCTFail("File mode cannot claim a read-only native exception") }
        catch { XCTAssertTrue(error.localizedDescription.contains("file-mode")) }
        XCTAssertEqual(try TradeConfirmationJournal(root: root).load(profile: context.profile, network: context.network), saved)
    }
    func testOrderNotificationWaitsForSelectedWalletMakerAndRejectsStaleIdentity() throws {
        let (root, _) = try fixture()
        let model = AppModel(daemon: DaemonProcess(root: root, security: isolatedNativeSecurity()))
        var settings = AppSettings(); settings.activeNetwork = "regtest"; settings.revision = 1
        var alice = Blakeswap_V1_WalletProfile(); alice.id = "alice"
        var bob = Blakeswap_V1_WalletProfile(); bob.id = "bob"; settings.wallets = [alice, bob]
        var status = DaemonStatus(); status.name = "alice"; status.network = "regtest"; status.pubkey = "alice-maker"
        XCTAssertTrue(model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation))
        model.openMonitoring(AlertDestination(network: "regtest", wallet: "bob", kind: "order", object: "same-parent-id"))
        XCTAssertEqual(model.page, "Market"); XCTAssertEqual(model.profile, "bob"); XCTAssertNil(model.activityDestination)
        let expected = model.generation
        XCTAssertFalse(model.acceptSnapshot(status, settings: settings, profile: "bob", generation: expected))
        XCTAssertNil(model.activityDestination)
        status.name = "bob"; status.pubkey = "bob-maker"
        XCTAssertTrue(model.acceptSnapshot(status, settings: settings, profile: "bob", generation: expected))
        XCTAssertEqual(model.activityDestination, .order("same-parent-id", maker: "bob-maker"))
        model.openMonitoring(AlertDestination(network: "regtest", wallet: "bob", kind: "order", object: "other"))
        let old = model.generation
        model.selectProfile("alice")
        XCTAssertFalse(model.acceptSnapshot(status, settings: settings, profile: "bob", generation: old))
        status.name = "alice"; status.pubkey = "alice-maker"
        XCTAssertTrue(model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation))
        XCTAssertNil(model.activityDestination, "A stale notification cannot select the same ID under another maker")
    }
    func testActivityUsesOneExactChildLookupAndMakerQualifiedParent() async throws {
        var row = Blakeswap_V1_ActivityRecord(); row.wallet = context.profile; row.network = context.network
        row.kind = "swap_claim"; row.swapID = "child"; row.orderID = "reused-parent"
        var calls = 0
        let target = try await ActivityDestination.parent(row, context: context, current: { self.context }) { method, raw in
            calls += 1; XCTAssertEqual(method, "record.get")
            let request = try Blakeswap_V1_RecordQuery(jsonUTF8Data: raw)
            XCTAssertEqual(request.kind, "swap"); XCTAssertEqual(request.id, row.swapID)
            XCTAssertEqual(request.expectedWallet, self.context.profile); XCTAssertEqual(request.expectedNetwork, self.context.network)
            var detail = Blakeswap_V1_RecordDetail(); detail.kind = "swap"; detail.id = row.swapID
            detail.archived = true; detail.swap.id = row.swapID; detail.swap.parentID = row.orderID; detail.swap.parentMaker = "foreign-maker"
            return try detail.serializedData()
        }
        XCTAssertEqual(calls, 1)
        XCTAssertEqual(target, .order("reused-parent", maker: "foreign-maker"))
        XCTAssertNotEqual(target, .order("reused-parent", maker: context.walletKey))
        XCTAssertNil(ActivityDestination.order("reused-parent", maker: ""))
    }
    func testActivityRejectsChangedContextOrMismatchedChildAndParent() async throws {
        for fault in ["context", "child", "parent", "maker"] {
            var current = context
            var row = Blakeswap_V1_ActivityRecord(); row.wallet = context.profile; row.network = context.network
            row.kind = "swap_claim"; row.swapID = "child"; row.orderID = "parent"
            do {
                _ = try await ActivityDestination.parent(row, context: context, current: { current }) { _, _ in
                    var detail = Blakeswap_V1_RecordDetail(); detail.kind = "swap"; detail.id = row.swapID
                    detail.swap.id = fault == "child" ? "foreign" : row.swapID
                    detail.swap.parentID = fault == "parent" ? "foreign" : row.orderID
                    detail.swap.parentMaker = fault == "maker" ? "" : "maker"
                    if fault == "context" { current = TradeContext(profile: "bob", network: "regtest", generation: 2, walletKey: "other") }
                    return try detail.serializedData()
                }
                XCTFail("Accepted \(fault)")
            } catch {}
        }
    }
}

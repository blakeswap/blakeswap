import XCTest
import SwiftUI
import AppKit
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class MarketTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "key")
    private func order(_ status: String) -> Blakeswap_V1_MarketOrder {
        var row = Blakeswap_V1_MarketOrder()
        row.offer.id = status; row.offer.maker = "maker"; row.offer.network = context.network
        row.offer.expires = Int64(Date().timeIntervalSince1970) + 300
        row.status = status; row.availability = status; row.own = true; row.side = "buy_btc"
        row.btcAmount = 9_999_999_999; row.blakeAmount = 10_000_000_000; row.rate = "1.00000000"
        row.eventID = "signed"; row.canCancel = status == "open"; row.swapIds = ["linked"]
        return row
    }
    private func page(_ rows: [Blakeswap_V1_MarketOrder] = []) -> Blakeswap_V1_MarketPage {
        var page = Blakeswap_V1_MarketPage(); page.wallet = context.profile; page.network = context.network
        page.records = rows; page.revision = "revision"; page.total = UInt32(rows.count)
        return page
    }
    func testAllStatusesPublicationAndOrderToSwapNavigation() throws {
        for status in ["open", "pending", "reserved", "filled", "cancelled", "expired", "refunded"] {
            let row = order(status)
            XCTAssertFalse(row.availabilityLabel.isEmpty)
            XCTAssertEqual(row.sideLabel, "You buy BTC")
            XCTAssertEqual(row.swapDestination, ActivityDestination(page: "Swaps", anchor: "swap/linked"))
            XCTAssertEqual(row.btcAmount, 9_999_999_999)
        }
        var row = order("open")
        row.availability = "stale"; XCTAssertTrue(row.availabilityLabel.contains("Stale"))
        row.publication = "local_committed"; XCTAssertTrue(row.publicationLabel.contains("pending"))
        row.publication = "relay_acknowledged"; XCTAssertTrue(row.publicationLabel.contains("acknowledged"))
        row.publication = "unknown"; XCTAssertTrue(row.publicationLabel.contains("unknown"))
        // Evaluate the native view tree with its actual context/model injection.
        let app = AppModel()
        let view = MarketView(context: context, root: "/unused").environmentObject(app)
        let host = NSHostingView(rootView: view)
        XCTAssertNotNil(host.rootView)
    }
    func testIndependentOwnerSideAmountFiltersAndEmptyState() async throws {
        for owner in ["all", "mine", "others"] {
            let model = MarketModel(context: context, root: "/unused") { method, data in
                XCTAssertEqual(method, "market.list")
                let q = try Blakeswap_V1_MarketQuery(jsonUTF8Data: data)
                XCTAssertEqual(q.owner, owner); XCTAssertEqual(q.side, "buy_btc")
                XCTAssertEqual(q.btcMin, 9_999_999_999); XCTAssertEqual(q.btcMax, 10_000_000_000)
                XCTAssertEqual(q.expectedWallet, self.context.profile); XCTAssertEqual(q.expectedNetwork, self.context.network)
                return try self.page().serializedData()
            }
            model.filters.owner = owner; model.filters.side = "buy_btc"
            model.filters.minimum = "9999999999"; model.filters.maximum = "10000000000"
            XCTAssertFalse(model.isEmpty)
            await model.load(current: { self.context })
            XCTAssertTrue(model.isEmpty); XCTAssertFalse(model.busy); XCTAssertNil(model.error)
        }
        for invalid in ["1.5", "-1", "10000000001", "9007199254740993"] {
            var filters = MarketFilters(); filters.minimum = invalid
            XCTAssertThrowsError(try filters.query(context: context))
        }
    }
    func testMarketPagingAndLateResultsCannotCrossScope() async throws {
        var calls = 0
        let model = MarketModel(context: context, root: "/unused") { _, data in
            let q = try Blakeswap_V1_MarketQuery(jsonUTF8Data: data)
            var p = self.page([self.order(calls == 0 ? "open" : "filled")]); p.total = 2
            if calls == 0 { XCTAssertEqual(q.offset, 0); p.more = true; p.nextOffset = 1 }
            else { XCTAssertEqual(q.offset, 1); XCTAssertEqual(q.revision, "revision"); p.nextOffset = 2 }
            calls += 1; return try p.serializedData()
        }
        await model.load(current: { self.context }); await model.load(more: true, current: { self.context })
        XCTAssertEqual(model.rows.map(\.status), ["open", "filled"]); XCTAssertFalse(model.more)
        for changed in ["wallet", "network", "generation", "filter"] {
            var current = context
            var resume: CheckedContinuation<Data, Error>?
            let delayed = MarketModel(context: context, root: "/unused") { _, _ in try await withCheckedThrowingContinuation { resume = $0 } }
            let task = Task { await delayed.load(current: { current }) }
            while resume == nil { await Task.yield() }
            switch changed {
            case "wallet": current = TradeContext(profile: "bob", network: "regtest", generation: 2, walletKey: "other")
            case "network": current = TradeContext(profile: "alice", network: "mainnet", generation: 2, walletKey: "key")
            case "generation": current = TradeContext(profile: "alice", network: "regtest", generation: 2, walletKey: "key")
            default: delayed.filters.side = "sell_btc"
            }
            resume?.resume(returning: try page([order("open")]).serializedData()); await task.value
            XCTAssertTrue(delayed.rows.isEmpty); XCTAssertNil(delayed.error)
        }
    }
    func testCancellationUsesExactOfferAndExcludesConcurrentReads() async throws {
        var resume: CheckedContinuation<Data, Error>?
        var methods: [String] = []
        let model = MarketModel(context: context, root: "/unused") { method, data in
            methods.append(method)
            if method == "offer.cancel" {
                let q = try Blakeswap_V1_CancelOfferRequest(jsonUTF8Data: data)
                XCTAssertEqual(q.id, "open"); XCTAssertEqual(q.expectedEventID, "signed")
                XCTAssertEqual(q.expectedWallet, self.context.profile); XCTAssertEqual(q.expectedNetwork, self.context.network)
                return try await withCheckedThrowingContinuation { resume = $0 }
            }
            return try self.page([self.order("cancelled")]).serializedData()
        }
        let task = Task { await model.cancel(self.order("open"), current: { self.context }) }
        while resume == nil { await Task.yield() }
        await model.load(current: { self.context }); await model.cancel(order("open"), current: { self.context })
        XCTAssertEqual(methods, ["offer.cancel"])
        resume?.resume(returning: Data()); await task.value
        XCTAssertEqual(methods, ["offer.cancel", "market.list"]); XCTAssertEqual(model.rows.first?.status, "cancelled")
    }
    func testReplacementSourceAndExpiryAreReviewedWithoutChangingJournalKind() async throws {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: url) }
        for action in ["replace", "recreate"] {
            var draft = Blakeswap_V1_TradeQuoteRequest(); draft.kind = "maker"; draft.orderAction = action
            draft.sourceOfferID = "old"; draft.sourceEventID = "signed"; draft.expires = 1_900_000_000
            let review = TradeReviewModel(context: context, root: url.path) { method, data in
                XCTAssertEqual(method, "trade.quote")
                let q = try Blakeswap_V1_TradeQuoteRequest(jsonUTF8Data: data)
                XCTAssertEqual(q.orderAction, action); XCTAssertEqual(q.sourceOfferID, "old"); XCTAssertEqual(q.sourceEventID, "signed"); XCTAssertEqual(q.expires, draft.expires)
                var result = Blakeswap_V1_TradeQuote(); result.kind = "maker"; result.wallet = self.context.profile; result.walletKey = self.context.walletKey; result.network = self.context.network
                result.orderAction = q.orderAction; result.sourceOfferID = q.sourceOfferID; result.sourceEventID = q.sourceEventID; result.offerExpires = q.expires
                result.token = "token"; result.revision = "revision"; result.expires = Int64(Date().timeIntervalSince1970) + 120; result.ready = true
                return try result.serializedData()
            }
            await review.review(draft, current: { self.context })
            XCTAssertEqual(review.quote?.kind, "maker"); XCTAssertEqual(review.quote?.orderAction, action)
            XCTAssertEqual(review.quote?.offerExpires, draft.expires)
        }
    }
}

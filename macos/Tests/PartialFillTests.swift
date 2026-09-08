import XCTest
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class PartialFillTests: XCTestCase {
    private let wallet = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "key")
    private func parent() -> Order {
        var order = Order(); order.version = 2; order.id = "parent"; order.maker = "maker"; order.network = "regtest"; order.revision = 8
        order.fillMode = "partial"; order.minFill = 400000; order.maxFill = 600000; order.sellAmount = 900000; order.buyAmount = 1170001; order.available = 900000
        return order
    }
    private func root() throws -> String {
        let path = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: path, withIntermediateDirectories: true)
        addTeardownBlock { try? FileManager.default.removeItem(at: path) }
        return path.path
    }
    func testExactWideCeilAndQuantityNeverRewritten() throws {
        XCTAssertEqual(try roundedFillBuy(total: 900000, buy: 1170001, quantity: 400000), 520001)
        XCTAssertEqual(try roundedFillBuy(total: 10_000_000_000, buy: 9_999_999_999, quantity: 9_999_999_999), 9_999_999_999)
        XCTAssertEqual(try roundedFillBuy(total: Int64.max, buy: Int64.max, quantity: Int64.max), Int64.max)
        XCTAssertThrowsError(try roundedFillBuy(total: 0, buy: 1, quantity: 1))
        XCTAssertThrowsError(try roundedFillBuy(total: 10, buy: 10, quantity: 11))
        var draft = TradeFillDraft(); draft.seed(quantity: 400000); draft.quantity = "456789"; draft.seed(quantity: 500000)
        XCTAssertEqual(draft.quantity, "456789")
        var request = Blakeswap_V2_TradeQuoteRequest(); try draft.apply(to: &request, order: parent())
        XCTAssertEqual(request.quantity, 456789); XCTAssertEqual(request.parentRevision, 8)
        draft.quantity = "1.5"; XCTAssertThrowsError(try draft.apply(to: &request, order: parent()))
        XCTAssertEqual(draft.quantity, "1.5")
        draft.quantity = "700000"; XCTAssertThrowsError(try draft.apply(to: &request, order: parent()))
    }
    func testExplicitCapsWholeBoundsAndSeparateExample() throws {
        var draft = TradeFillDraft(), request = Blakeswap_V2_TradeQuoteRequest(); request.sellAmount = 900000
        XCTAssertThrowsError(try draft.apply(to: &request, order: nil))
        draft.btcFees = "65000"; draft.blakeFees = "40000"
        try draft.apply(to: &request, order: nil)
        XCTAssertEqual(request.minFill, 900000); XCTAssertEqual(request.maxFill, 900000)
        XCTAssertEqual(request.bountyBudgets, ["btc":0,"blake":0])
        draft.mode = "partial"; draft.minimum = "400000"; draft.maximum = "600000"
        try draft.apply(to: &request, order: nil)
        XCTAssertEqual(request.minFill, 400000); XCTAssertEqual(request.feeBudgets["blake"], 40000)
        var quote = Blakeswap_V2_TradeQuote(); quote.kind = "maker"; quote.fillMode = "partial"; quote.fundingReserve = 13000; quote.fees.fundingFee = 6500
        var child = Blakeswap_V2_FillPreview(); child.quantity = 400000; child.buyAmount = 520001
        var outcome = Blakeswap_V2_TradeOutcome(); outcome.netMin = 500001; child.outcomes = [outcome]; quote.exampleFill = child
        let presentation = TradeEconomicsPresentation(quote: quote)
        XCTAssertEqual(presentation.fundingReserve, 13000); XCTAssertEqual(presentation.receivedLabel, "Price-reference amount")
        XCTAssertEqual(presentation.displayedOutcomes, child.outcomes); XCTAssertTrue(quote.outcomes.isEmpty)
    }
    func testDelayedDraftAndAlteredQuantityRepliesRefused() async throws {
        var request = Blakeswap_V2_TradeQuoteRequest(); request.kind = "taker"; request.id = "parent"; request.maker = "maker"; request.quantity = 400000; request.parentRevision = 8
        var reply = Blakeswap_V2_TradeQuote(); reply.kind = "taker"; reply.wallet = wallet.profile; reply.walletKey = wallet.walletKey; reply.network = wallet.network
        reply.quantity = 400000; reply.parentRevision = 8; reply.offerID = "parent"; reply.offerMaker = "maker"; reply.offerEventID = "event"
        var continuation: CheckedContinuation<Data, Error>?
        let review = TradeReviewModel(context: wallet, root: try root()) { _, _ in try await withCheckedThrowingContinuation { continuation = $0 } }
        let pending = Task { await review.review(request, expectedEventID: "event", current: { self.wallet }) }
        while continuation == nil { await Task.yield() }
        review.invalidateDraft(); continuation?.resume(returning: try reply.serializedData()); await pending.value
        XCTAssertNil(review.quote); XCTAssertNil(review.error)
        for field in ["quantity", "revision", "event"] {
            var changed = reply
            if field == "quantity" { changed.quantity += 1 }; if field == "revision" { changed.parentRevision += 1 }; if field == "event" { changed.offerEventID = "other" }
            let model = TradeReviewModel(context: wallet, root: try root()) { _, _ in try changed.serializedData() }
            await model.review(request, expectedEventID: "event", current: { self.wallet })
            XCTAssertNil(model.quote); XCTAssertNotNil(model.error)
        }
        let good = TradeReviewModel(context: wallet, root: try root()) { _, _ in try reply.serializedData() }
        await good.review(request, expectedEventID: "event", current: { self.wallet }); XCTAssertNotNil(good.quote)
    }
    func testExplicitParentRefreshRetainsRawQuantityAndIdentity() throws {
        let previous = parent()
        var row = Blakeswap_V2_MarketOrder(); row.offer = previous; row.offer.revision += 1; row.offer.available = 300000; row.eventID = "new-event"
        var draft = TradeFillDraft(); draft.seed(quantity: 400000); draft.quantity = "456789"
        try validateRefreshedParent(previous, refreshed: row)
        draft.seed(quantity: row.suggestedQuantity)
        XCTAssertEqual(draft.quantity, "456789")
        var request = Blakeswap_V2_TradeQuoteRequest()
        XCTAssertThrowsError(try draft.apply(to: &request, order: row.offer))
        row.offer.maker = "foreign"; XCTAssertThrowsError(try validateRefreshedParent(previous, refreshed: row))
        row.offer = previous; row.offer.revision = 1; XCTAssertThrowsError(try validateRefreshedParent(previous, refreshed: row))
    }
    func testMakerCapsAndExampleCannotChangeInReply() async throws {
        var request = Blakeswap_V2_TradeQuoteRequest(); request.kind = "maker"; request.fillMode = "partial"; request.minFill = 400000; request.maxFill = 600000
        request.feeBudgets = ["btc":65000,"blake":40000]; request.bountyBudgets = ["btc":0,"blake":0]
        var good = Blakeswap_V2_TradeQuote(); good.kind = request.kind; good.wallet = wallet.profile; good.walletKey = wallet.walletKey; good.network = wallet.network
        good.fillMode = request.fillMode; good.minFill = request.minFill; good.maxFill = request.maxFill; good.feeBudgets = request.feeBudgets; good.bountyBudgets = request.bountyBudgets; good.exampleFill.quantity = 400000
        for change in ["fees", "bounty", "bounds", "example", "aggregate"] {
            var reply = good
            switch change {
            case "fees": reply.feeBudgets["btc"] = 65001
            case "bounty": reply.bountyBudgets.removeValue(forKey: "btc")
            case "bounds": reply.maxFill = 700000
            case "example": reply.clearExampleFill()
            default: reply.outcomes = [Blakeswap_V2_TradeOutcome()]
            }
            let model = TradeReviewModel(context: wallet, root: try root()) { _, _ in try reply.serializedData() }
            await model.review(request, current: { self.wallet }); XCTAssertNil(model.quote); XCTAssertNotNil(model.error)
        }
    }
    func testDelayedPageCannotApplyToChangedWalletOrNewRequest() async throws {
        var current = wallet
        let changed = FillHistoryModel(context: ParentFillContext(wallet: wallet, maker: "maker", parentID: "parent")) { _, raw in
            let request = try Blakeswap_V2_FillQuery(jsonUTF8Data: raw)
            current = TradeContext(profile: "bob", network: "regtest", generation: 2, walletKey: "other")
            return try self.page(request).serializedData()
        }
        await changed.load(current: { current }); XCTAssertTrue(changed.rows.isEmpty)
        var resume: CheckedContinuation<Data, Error>?
        var oldRequest = Blakeswap_V2_FillQuery()
        var calls = 0
        let model = FillHistoryModel(context: ParentFillContext(wallet: wallet, maker: "maker", parentID: "parent")) { _, raw in
            let request = try Blakeswap_V2_FillQuery(jsonUTF8Data: raw); calls += 1
            if calls == 1 { oldRequest = request; return try await withCheckedThrowingContinuation { resume = $0 } }
            var result = self.page(request); result.revision = "new-frozen"; return try result.serializedData()
        }
        let old = Task { await model.load(current: { self.wallet }) }
        while resume == nil { await Task.yield() }
        await model.load(refresh: true, current: { self.wallet })
        let latest = model.rows
        var stale = page(oldRequest); stale.records[0].quantity = 1
        resume?.resume(returning: try stale.serializedData()); await old.value
        XCTAssertEqual(model.rows, latest); XCTAssertNil(model.error)
    }
    private func page(_ request: Blakeswap_V2_FillQuery) -> Blakeswap_V2_FillPage {
        var page = Blakeswap_V2_FillPage(); page.wallet = wallet.profile; page.network = wallet.network; page.parentMaker = "maker"; page.parentID = "parent"; page.revision = "frozen"; page.total = 1001
        let end = min(request.offset + request.limit, page.total)
        page.records = (request.offset..<end).map { number in
            var row = Blakeswap_V2_FillSummary(); row.id = String(format: "child-%04d", number); row.parentMaker = "maker"; row.parentID = "parent"; row.quantity = 400000; row.allocatedQuantity = 400000; row.allocationKnown = true; row.disposition = "released"; row.archived = number.isMultiple(of: 2); row.monitoringRequired = row.archived; return row
        }
        page.nextOffset = end; page.more = end < page.total; return page
    }
    func testFillHistoryBoundedPagesAndExactDetail() async throws {
        var requests: [Blakeswap_V2_FillQuery] = []
        let model = FillHistoryModel(context: ParentFillContext(wallet: wallet, maker: "maker", parentID: "parent")) { method, raw in
            if method == "record.get" {
                let request = try Blakeswap_V2_RecordQuery(jsonUTF8Data: raw)
                XCTAssertEqual(request.expectedWallet, self.wallet.profile); XCTAssertEqual(request.expectedNetwork, self.wallet.network)
                var detail = Blakeswap_V2_RecordDetail(); detail.kind = "swap"; detail.id = request.id; detail.swap.id = request.id; detail.swap.parentID = "parent"; detail.swap.parentMaker = "maker"; detail.archived = true; detail.monitoringRequired = true
                return try detail.serializedData()
            }
            let request = try Blakeswap_V2_FillQuery(jsonUTF8Data: raw); requests.append(request)
            XCTAssertEqual(method, "fills.list"); XCTAssertEqual(request.parentMaker, "maker"); XCTAssertEqual(request.parentID, "parent")
            return try self.page(request).serializedData()
        }
        await model.load(current: { wallet })
        while model.more { await model.next(current: { wallet }); XCTAssertLessThanOrEqual(model.rows.count, 100) }
        XCTAssertEqual(model.total, 1001); XCTAssertEqual(model.rows.count, 1); XCTAssertEqual(requests.count, 11)
        XCTAssertTrue(requests.dropFirst().allSatisfy { $0.revision == "frozen" })
        await model.previous(current: { wallet }); XCTAssertEqual(model.offset, 900)
        let row = try XCTUnwrap(model.rows.first); XCTAssertTrue(row.allocationLabel.contains("400000"))
        await model.select(row, current: { wallet }); XCTAssertTrue(model.selected?.monitoringRequired == true)
        var retired = row; retired.disposition = "retired"; retired.allocatedQuantity = 0; XCTAssertTrue(retired.allocationLabel.contains("0 sell"))
        retired.allocationKnown = false; XCTAssertEqual(retired.allocationLabel, "Current maker allocation unknown")
    }
    func testForeignOrStaleFillPagesNeverApply() async throws {
        for alteration in ["maker", "parent", "revision", "offset", "wallet"] {
            var calls = 0
            let model = FillHistoryModel(context: ParentFillContext(wallet: wallet, maker: "maker", parentID: "parent")) { _, raw in
                let request = try Blakeswap_V2_FillQuery(jsonUTF8Data: raw); var result = self.page(request); calls += 1
                if calls > 1 {
                    switch alteration { case "maker": result.records[0].parentMaker = "foreign"
                    case "parent": result.parentID = "foreign"
                    case "revision": result.revision = "new"
                    case "offset": result.nextOffset = 1
                    default: result.wallet = "foreign" }
                }
                return try result.serializedData()
            }
            await model.load(current: { wallet }); let original = model.rows
            await model.next(current: { wallet }); XCTAssertEqual(model.rows, original); XCTAssertNotNil(model.error)
        }
    }
}

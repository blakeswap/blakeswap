import XCTest
@testable import Blakeswap

final class TradeManagementDraftTests: XCTestCase {
    private func source(mode: String = "partial", available: Int64 = 500_000) -> Blakeswap_V2_MarketOrder {
        var row = Blakeswap_V2_MarketOrder()
        row.own = true; row.eventID = "signed-revision-eight"
        row.offer.version = 2; row.offer.revision = 8; row.offer.network = "regtest"
        row.offer.id = "parent"; row.offer.maker = "maker"; row.offer.sell = "blake"
        row.offer.sellAmount = 900_000; row.offer.buyAmount = 1_170_001
        row.offer.fillMode = mode; row.offer.minFill = mode == "whole" ? 900_000 : 400_000
        row.offer.maxFill = mode == "whole" ? 900_000 : 600_000
        // A delayed public revision still shows the original total. It must not
        // override the authenticated local conserved summary.
        row.offer.available = 900_000
        row.quantities.total = 900_000; row.quantities.available = available
        row.quantities.reserved = 900_000 - available
        return row
    }

    func testReplacementSeedsAuthoritativeRemainderAndExactRoundedReference() throws {
        let seed = try TradeManagementSeed(action: "replace", source: source())
        XCTAssertEqual(seed.sell, "blake"); XCTAssertEqual(seed.sellAmount, 500_000)
        XCTAssertEqual(seed.buyAmount, 650_001)
        XCTAssertEqual(seed.fill.mode, "partial")
        XCTAssertEqual(seed.fill.minimum, "400000"); XCTAssertEqual(seed.fill.maximum, "500000")
        XCTAssertEqual(seed.sourceOfferID, "parent"); XCTAssertEqual(seed.sourceEventID, "signed-revision-eight")
        for available: Int64 in [100_000, 300_000, 500_000, 700_000, 900_000] {
            let seed = try TradeManagementSeed(action: "replace", source: source(available: available))
            let minimum = try TradeFillDraft.amount(seed.fill.minimum, positive: true)
            let maximum = try TradeFillDraft.amount(seed.fill.maximum, positive: true)
            XCTAssertLessThanOrEqual(maximum, available); XCTAssertLessThanOrEqual(minimum, maximum)
            XCTAssertLessThanOrEqual(1 + (available - 1) / maximum, available / minimum)
            XCTAssertGreaterThanOrEqual(try roundedFillBuy(total: available, buy: seed.buyAmount, quantity: minimum), 100_000)
        }
    }

    func testWholeReplacementAndRecreateKeepOriginalFullAmounts() throws {
        let whole = try TradeManagementSeed(action: "replace", source: source(mode: "whole", available: 900_000))
        XCTAssertEqual(whole.sellAmount, 900_000); XCTAssertEqual(whole.buyAmount, 1_170_001)
        XCTAssertEqual(whole.fill.minimum, "900000"); XCTAssertEqual(whole.fill.maximum, "900000")
        for mode in ["whole", "partial"] {
            var row = source(mode: mode, available: 0)
            row.clearQuantities() // Recreate needs no current availability grant.
            let recreate = try TradeManagementSeed(action: "recreate", source: row)
            XCTAssertEqual(recreate.sellAmount, 900_000); XCTAssertEqual(recreate.buyAmount, 1_170_001)
            XCTAssertEqual(recreate.fill.mode, mode)
            XCTAssertEqual(recreate.fill.minimum, String(row.offer.minFill))
            XCTAssertEqual(recreate.fill.maximum, String(row.offer.maxFill))
            XCTAssertNoThrow(try recreate.validate(sell: "btc", amount: "1000000"))
        }
    }

    func testUnknownZeroAndMalformedRemaindersNeverCreateDrafts() {
        for kind in ["absent", "zero", "negative", "excess", "total", "negative-bin", "unconserved", "overflow", "whole-partial", "dust", "foreign", "event", "version", "bounds"] {
            var row = source()
            switch kind {
            case "absent": row.clearQuantities()
            case "zero": row.quantities.available = 0; row.quantities.reserved = 900_000
            case "negative": row.quantities.available = -1; row.quantities.reserved = 900_001
            case "excess": row.quantities.available = 900_001; row.quantities.reserved = 0
            case "total": row.quantities.total = 899_999
            case "negative-bin": row.quantities.filled = -1; row.quantities.reserved += 1
            case "unconserved": row.quantities.committed = 1
            case "overflow": row.quantities.reserved = Int64.max; row.quantities.filled = Int64.max
            case "whole-partial": row.offer.fillMode = "whole"; row.offer.minFill = 900_000; row.offer.maxFill = 900_000
            case "dust": row.quantities.available = 99_999; row.quantities.reserved = 800_001
            case "foreign": row.own = false
            case "event": row.eventID = ""
            case "version": row.offer.version = 1
            default: row.offer.minFill = 700_000
            }
            XCTAssertThrowsError(try TradeManagementSeed(action: "replace", source: row), kind)
        }
    }

    func testReplacementAmountAssetAndSourceStayBoundWhileFreshTermsAreEditable() throws {
        let seed = try TradeManagementSeed(action: "replace", source: source())
        for amount in ["900000", "500001", "499999", "1.5", "", "-1"] {
            XCTAssertThrowsError(try seed.validate(sell: "blake", amount: amount))
        }
        XCTAssertThrowsError(try seed.validate(sell: "btc", amount: "500000"))
        var request = Blakeswap_V2_TradeQuoteRequest()
        request.kind = "maker"; request.sell = seed.sell; request.sellAmount = seed.sellAmount
        request.buyAmount = 700_001 // Explicit new price is allowed.
        var fill = seed.fill
        XCTAssertEqual(fill.btcFees, ""); XCTAssertEqual(fill.blakeFees, "")
        XCTAssertEqual(fill.btcBounty, ""); XCTAssertEqual(fill.blakeBounty, "")
        XCTAssertThrowsError(try fill.apply(to: &request, order: nil))
        fill.btcFees = "20000"; fill.blakeFees = "22000"
        XCTAssertThrowsError(try fill.apply(to: &request, order: nil))
        fill.btcBounty = "0"; fill.blakeBounty = "0"
        fill.mode = "whole"
        try seed.applySource(to: &request); try fill.apply(to: &request, order: nil)
        XCTAssertEqual(request.sellAmount, 500_000); XCTAssertEqual(request.buyAmount, 700_001)
        XCTAssertEqual(request.minFill, 500_000); XCTAssertEqual(request.maxFill, 500_000)
        XCTAssertEqual(request.orderAction, "replace"); XCTAssertEqual(request.sourceOfferID, "parent")
        XCTAssertEqual(request.sourceEventID, "signed-revision-eight")
        fill.mode = "partial"; fill.minimum = "100000"; fill.maximum = "300000"
        try fill.apply(to: &request, order: nil)
        XCTAssertEqual(request.minFill, 100_000); XCTAssertEqual(request.maxFill, 300_000)
        request.sellAmount += 1
        XCTAssertThrowsError(try seed.applySource(to: &request))
    }

    func testWideIntegerRoundingAndSubminimumPriceReference() throws {
        var row = source(available: 500_000)
        row.offer.sellAmount = 10_000_000_000; row.offer.buyAmount = 9_999_999_999
        row.quantities.total = 10_000_000_000; row.quantities.available = 9_999_999_999; row.quantities.reserved = 1
        let seed = try TradeManagementSeed(action: "replace", source: row)
        XCTAssertEqual(seed.buyAmount, 9_999_999_999)
        row = source(available: 100_000); row.offer.buyAmount = 100_000
        XCTAssertThrowsError(try TradeManagementSeed(action: "replace", source: row))
    }
}

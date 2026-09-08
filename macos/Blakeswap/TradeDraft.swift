import Foundation

// Exact preview only. The daemon remains authoritative for economic tails,
// current funds, fees, protection and concurrent parent availability.
func roundedFillBuy(total: Int64, buy: Int64, quantity: Int64) throws -> Int64 {
    guard total > 0, buy > 0, quantity > 0, quantity <= total else { throw RPCError.message("Enter a positive fill within the parent amount.") }
    let product = UInt64(quantity).multipliedFullWidth(by: UInt64(buy))
    let divisor = UInt64(total)
    guard product.high < divisor else { throw RPCError.message("Fill amount overflows supported integers.") }
    let division = divisor.dividingFullWidth(product)
    let (rounded, overflow) = division.quotient.addingReportingOverflow(division.remainder == 0 ? 0 : 1)
    guard !overflow, rounded <= UInt64(Int64.max) else { throw RPCError.message("Fill amount overflows supported integers.") }
    return Int64(rounded)
}

struct TradeFillDraft: Equatable {
    var mode = "whole"
    var minimum = ""
    var maximum = ""
    var quantity = ""
    var btcFees = ""
    var blakeFees = ""
    var btcBounty = "0"
    var blakeBounty = "0"
    private(set) var seeded = false

    mutating func seed(quantity suggested: Int64) {
        guard !seeded else { return }
        seeded = true
        if quantity.isEmpty, suggested > 0 { quantity = String(suggested) }
    }
    static func amount(_ text: String, positive: Bool = false) throws -> Int64 {
        guard !text.isEmpty, text.utf8.allSatisfy({ (48...57).contains($0) }),
              let amount = Int64(text), amount >= (positive ? 1 : 0) else {
            throw RPCError.message("Enter exact whole satoshis; every limit must be explicit.")
        }
        return amount
    }
    func apply(to request: inout Blakeswap_V2_TradeQuoteRequest, order: Order?) throws {
        if let order {
            let q = try Self.amount(quantity, positive: true)
            guard order.version == 2, order.revision > 0, q >= order.minFill, q <= order.maxFill, q <= order.available else {
                throw RPCError.message("Fill quantity is outside this signed revision's available bounds. Refresh and review the parent explicitly.")
            }
            _ = try roundedFillBuy(total: order.sellAmount, buy: order.buyAmount, quantity: q)
            request.quantity = q; request.parentRevision = order.revision
            request.fillMode = order.fillMode; request.minFill = order.minFill; request.maxFill = order.maxFill
        } else {
            guard mode == "whole" || mode == "partial" else { throw RPCError.message("Select whole or partial fills.") }
            request.fillMode = mode
            request.minFill = mode == "whole" ? request.sellAmount : try Self.amount(minimum, positive: true)
            request.maxFill = mode == "whole" ? request.sellAmount : try Self.amount(maximum, positive: true)
            guard request.minFill >= 100_000, request.minFill <= request.maxFill, request.maxFill <= request.sellAmount else { throw RPCError.message("Minimum and maximum fill must fit the parent amount.") }
            request.feeBudgets = ["btc": try Self.amount(btcFees, positive: true), "blake": try Self.amount(blakeFees, positive: true)]
            request.bountyBudgets = ["btc": try Self.amount(btcBounty), "blake": try Self.amount(blakeBounty)]
        }
    }
}

// Parent and example economics are different views, never summed together.
struct TradeEconomicsPresentation {
    let quote: Blakeswap_V2_TradeQuote
    var partialParent: Bool { quote.kind == "maker" && quote.fillMode == "partial" }
    var displayedOutcomes: [Blakeswap_V2_TradeOutcome] { partialParent ? quote.exampleFill.outcomes : quote.outcomes }
    var fundingReserve: Int64 { partialParent ? quote.fundingReserve : quote.fees.fundingFee }
    var receivedLabel: String { partialParent ? "Price-reference amount" : "Receive principal" }
}

func validateRefreshedParent(_ previous: Order, refreshed: Blakeswap_V2_MarketOrder) throws {
    let next = refreshed.offer
    guard next.version == 2, next.network == previous.network, next.maker == previous.maker,
          next.id == previous.id, next.revision >= previous.revision, !refreshed.eventID.isEmpty else {
        throw RPCError.message("Refreshed order does not match the selected parent identity.")
    }
}

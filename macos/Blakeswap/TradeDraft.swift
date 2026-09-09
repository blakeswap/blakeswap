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
    func apply(to request: inout Blakeswap_V1_TradeQuoteRequest, order: Order?) throws {
        if let order {
            let q = try Self.amount(quantity, positive: true)
            guard order.version == 1, order.revision > 0, q >= order.minFill, q <= order.maxFill, q <= order.available else {
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

// A replacement is a new review of the locally known remainder. Public
// availability may lag the private ledger and must never seed this amount.
struct TradeManagementSeed {
    let action: String
    let sell: String
    let sellAmount: Int64
    let buyAmount: Int64
    let sourceOfferID: String
    let sourceEventID: String
    let fill: TradeFillDraft

    init(action: String, source: Blakeswap_V1_MarketOrder) throws {
        let original = source.offer
        let range: ClosedRange<Int64> = 100_000...10_000_000_000
        guard action == "replace" || action == "recreate", source.own,
              original.version == 1, original.revision > 0,
              !original.id.isEmpty, !original.maker.isEmpty, !source.eventID.isEmpty,
              original.sell == "btc" || original.sell == "blake",
              range.contains(original.sellAmount), range.contains(original.buyAmount),
              original.fillMode == "whole" || original.fillMode == "partial",
              original.minFill >= 100_000, original.minFill <= original.maxFill,
              original.maxFill <= original.sellAmount,
              original.fillMode != "whole" || (original.minFill == original.sellAmount && original.maxFill == original.sellAmount) else {
            throw RPCError.message("The source order is incomplete. Refresh your market history before reviewing it.")
        }
        var amount = original.sellAmount
        if action == "replace" {
            guard source.hasQuantities, source.quantities.total == original.sellAmount else {
                throw RPCError.message("The available remainder is unknown. Refresh your market history before replacing this order.")
            }
            let quantities = source.quantities
            var unaccounted = quantities.total
            for bin in [quantities.available, quantities.reserved, quantities.committed, quantities.filled, quantities.released] {
                guard bin >= 0, bin <= unaccounted else {
                    throw RPCError.message("The source quantity summary is inconsistent. Refresh before replacing this order.")
                }
                unaccounted -= bin
            }
            guard unaccounted == 0, range.contains(quantities.available),
                  original.fillMode != "whole" || quantities.available == original.sellAmount else {
                throw RPCError.message("No valid available remainder can be replaced. Refresh your market history.")
            }
            amount = quantities.available
        }
        let reference = try roundedFillBuy(total: original.sellAmount, buy: original.buyAmount, quantity: amount)
        guard range.contains(reference) else {
            throw RPCError.message("The remainder cannot form a new order at this price reference.")
        }
        var draft = TradeFillDraft()
        draft.mode = original.fillMode
        var minimum = min(original.minFill, amount), maximum = min(original.maxFill, amount)
        if action == "replace", draft.mode == "partial" {
            let leastParts = 1 + (amount - 1) / maximum
            // The new parent's rounded reference can change its economic
            // interval. Seed one legal fill if clipped old bounds cannot cover
            // it; these are editable draft values, never a retained grant.
            let minimumReceive = try roundedFillBuy(total: amount, buy: reference, quantity: minimum)
            if leastParts > amount / minimum || minimumReceive < 100_000 {
                minimum = amount; maximum = amount
            }
        }
        draft.minimum = String(minimum); draft.maximum = String(maximum)
        // Neither a public order nor its old private authorization supplies
        // fresh limits for this new parent, including conditional bounties.
        draft.btcFees = ""; draft.blakeFees = ""; draft.btcBounty = ""; draft.blakeBounty = ""
        self.action = action; sell = original.sell; sellAmount = amount; buyAmount = reference
        sourceOfferID = original.id; sourceEventID = source.eventID; fill = draft
    }

    func validate(sell: String, amount: String) throws {
        if action == "replace" {
            guard sell == self.sell, try TradeFillDraft.amount(amount, positive: true) == sellAmount else {
                throw RPCError.message("A replacement must use exactly the available remainder in its original sell asset.")
            }
        }
    }

    func applySource(to request: inout Blakeswap_V1_TradeQuoteRequest) throws {
        try validate(sell: request.sell, amount: String(request.sellAmount))
        request.orderAction = action; request.sourceOfferID = sourceOfferID; request.sourceEventID = sourceEventID
    }
}

// Parent and example economics are different views, never summed together.
struct TradeEconomicsPresentation {
    let quote: Blakeswap_V1_TradeQuote
    var partialParent: Bool { quote.kind == "maker" && quote.fillMode == "partial" }
    var displayedOutcomes: [Blakeswap_V1_TradeOutcome] { partialParent ? quote.exampleFill.outcomes : quote.outcomes }
    var fundingReserve: Int64 { partialParent ? quote.fundingReserve : quote.fees.fundingFee }
    var receivedLabel: String { partialParent ? "Price-reference amount" : "Receive principal" }
}

func validateRefreshedParent(_ previous: Order, refreshed: Blakeswap_V1_MarketOrder) throws {
    let next = refreshed.offer
    guard next.version == 1, next.network == previous.network, next.maker == previous.maker,
          next.id == previous.id, next.revision >= previous.revision, !refreshed.eventID.isEmpty else {
        throw RPCError.message("Refreshed order does not match the selected parent identity.")
    }
}

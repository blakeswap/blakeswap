import Foundation
import SwiftProtobuf

typealias Order = Blakeswap_V1_Offer
typealias HTLC = Blakeswap_V1_HTLC
typealias Swap = Blakeswap_V1_Swap
typealias DaemonStatus = Blakeswap_V1_Status
typealias AppSettings = Blakeswap_V1_Settings
typealias EnvironmentSettings = Blakeswap_V1_Environment
typealias NodeSettings = Blakeswap_V1_Node

extension Blakeswap_V1_Offer: Identifiable {
    var buy: String { sell == "btc" ? "blake" : "btc" }
    func protectionLabel(viewer: String) -> String? {
        guard !viewer.isEmpty, maker == viewer else { return nil }
        return towerBps > 0 ? "Watchtower: \(percentage(towerBps)) only if used" : "No protection"
    }
}
extension Blakeswap_V1_Swap: Identifiable {
    var feeLabel: String {
        let payments = towerPayments.keys.sorted().compactMap { chain -> String? in
            guard let amount = towerPayments[chain], amount > 0 else { return nil }
            return "\(amount) \(symbol(chain)) sats"
        }
        return payments.isEmpty ? "0 sats" : payments.joined(separator: " + ")
    }
}
extension Blakeswap_V1_Environment: Identifiable { var id: String { network } }
extension Blakeswap_V1_Environment {
    var rescueFeePercent: Double {
        get { Double(rescueFeeBasisPoints) / 100 }
        set {
            guard newValue.isFinite else { return }
            rescueFeeBasisPoints = Int64((min(10, max(0.01, newValue)) * 100).rounded())
        }
    }
    var rescueFeeBasisPoints: Int64 {
        get { rescueFeeBps == 0 ? 50 : rescueFeeBps }
        set { rescueFeeBps = newValue }
    }
}
func units(_ sats: Int64) -> String { "\(sats / 100_000_000).\(String(format: "%08lld", sats % 100_000_000))" }
func symbol(_ chain: String) -> String { chain == "btc" ? "BTC" : "BLAKE" }
func percentage(_ bps: Int64) -> String { String(format: "%.2f%%", Double(bps) / 100) }
enum RPCError: LocalizedError {
    case message(String)
    var errorDescription: String? { if case .message(let message) = self { return message }; return nil }
}

func locktimeLabel(_ value: UInt32) -> String {
    value < 500_000_000 ? "Block #\(value)" : Date(timeIntervalSince1970: TimeInterval(value)).formatted(date: .abbreviated, time: .shortened)
}

enum OrderFilter: String, CaseIterable {
    case all = "All open orders"
    case mine = "My open orders"
    case others = "Other open orders"
    var title: String { switch self { case .all: "All orders"; case .mine: "My orders"; case .others: "Other orders" } }
    var key: String { switch self { case .all: "all"; case .mine: "mine"; case .others: "others" } }
    func orders(in status: DaemonStatus) -> [Order] {
        status.orders.filter { order in
            order.status == "open" && (self == .all || (order.maker == status.pubkey) == (self == .mine))
        }
    }
}

extension Blakeswap_V1_Offer {
    var bookID: String { "\(maker):\(id)" }
}
extension Blakeswap_V1_Tower: Identifiable {
    var id: String { pubkey }
    var label: String { "\(name.isEmpty ? String(npub.prefix(16)) + "…" : name) · \(percentage(bps))" }
}
extension Blakeswap_V1_Status {
    var offerFundingFee: Int64 { fundingFee > 0 ? fundingFee : 2_000 }
    func available(_ chain: String) -> Int64 { funds[chain]?.unlockedConfirmed ?? 0 }
    func canSell(_ chain: String) -> Bool { available(chain) >= 100_000 + offerFundingFee }
    func offerValidation(sell: String, sellAmount: String, buyAmount: String, fee: Int64? = nil) -> String? {
        let offerFundingFee = fee ?? self.offerFundingFee
        guard ["btc", "blake"].contains(sell), !pubkey.isEmpty else { return "Waiting for your wallet balance." }
        guard let a = Int64(sellAmount), let b = Int64(buyAmount),
              (100_000...10_000_000_000).contains(a), (100_000...10_000_000_000).contains(b) else {
            return "Enter whole satoshi amounts from 100,000 to 10 billion."
        }
        let balance = available(sell)
        guard balance > 0 else { return "No unlocked confirmed \(symbol(sell)) is available. Cancel an open order to release its coins, or deposit and wait for confirmation." }
        guard a <= balance - offerFundingFee else {
            return "Insufficient \(symbol(sell)): \(balance) sats available. Leave \(offerFundingFee) sats for the funding fee."
        }
        return nil
    }
}

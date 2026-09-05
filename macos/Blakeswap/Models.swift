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

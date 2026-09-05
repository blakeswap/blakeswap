import Foundation

struct Order: Decodable, Identifiable {
    let id: String
    let maker: String
    let sell: String
    let sellAmount: Int64
    let buyAmount: Int64
    let towerBps: Int64
    let expires: Int64
    let status: String
    var buy: String { sell == "btc" ? "blake" : "btc" }
}

struct HTLC: Decodable {
    let chain: String
    let amount: Int64
    let refundHeight: UInt32
    let txid: String?
}

struct Swap: Decodable, Identifiable {
    let id: String
    let role: String
    let stage: String
    let error: String?
    let long: HTLC
    let short: HTLC
    let longSpend: String?
    let shortSpend: String?
    let longConfirmations: Int
    let shortConfirmations: Int
    let towerPaid: Int64
    let towerPayments: [String: Int64]
    let towerReady: Bool
    let towerEnabled: Bool
    let secretRevealed: Bool
    let takeover: UInt32
    let revealBefore: UInt32
    var feeLabel: String {
        let payments = towerPayments.keys.sorted().compactMap { chain -> String? in
            guard let amount = towerPayments[chain], amount > 0 else { return nil }
            return "\(amount) \(symbol(chain)) sats"
        }
        return payments.isEmpty ? "0 sats" : payments.joined(separator: " + ")
    }
}

struct TowerQuote: Decodable {
    let pubkey: String
    let bps: Int64
}

struct DaemonStatus: Decodable {
    let name: String
    let mode: String
    let pubkey: String
    let addresses: [String: String]
    let balances: [String: Int64]
    let heights: [String: UInt32]
    let paused: Bool
    let orders: [Order]
    let swaps: [Swap]
    let pendingMessages: Int
    let lastError: String
    let tower: TowerQuote
}

func units(_ sats: Int64) -> String {
    "\(sats / 100_000_000).\(String(format: "%08lld", sats % 100_000_000))"
}
func symbol(_ chain: String) -> String { chain == "btc" ? "BTC" : "BLAKE" }
func percentage(_ bps: Int64) -> String { String(format: "%.2f%%", Double(bps) / 100) }

enum RPCError: LocalizedError {
    case message(String)
    var errorDescription: String? { if case .message(let message) = self { return message }; return nil }
}

import Foundation
import SwiftProtobuf

struct ActivityFilters: Equatable {
    var kind = ""
    var status = ""
    var chain = ""
    var from: Int64 = 0
    var to: Int64 = 0
    func query(context: TradeContext) -> Blakeswap_V1_ActivityQuery {
        var query = Blakeswap_V1_ActivityQuery()
        query.expectedWallet = context.profile; query.expectedNetwork = context.network
        query.kind = kind; query.status = status; query.chain = chain; query.from = from; query.to = to; query.limit = 100
        return query
    }
}

struct ActivityDestination: Equatable {
    let page: String
    let anchor: String
    static func order(_ id: String) -> Self? { id.isEmpty ? nil : Self(page: "Market", anchor: "order/" + id) }
    static func swap(_ id: String) -> Self? { id.isEmpty ? nil : Self(page: "Swaps", anchor: "swap/" + id) }
    static func send(_ id: String) -> Self? { id.isEmpty ? nil : Self(page: "Wallet", anchor: "send/" + id) }
}

enum ActivityPhase: Equatable { case idle, loading, loaded, failed }

@MainActor
final class ActivityModel: ObservableObject {
    @Published var filters = ActivityFilters()
    @Published private(set) var records: [Blakeswap_V1_ActivityRecord] = []
    @Published private(set) var phase: ActivityPhase = .idle
    @Published private(set) var error: String?
    @Published private(set) var indexing: [String] = []
    @Published private(set) var total: UInt32 = 0
    @Published private(set) var nextCursor: UInt32 = 0
    @Published private(set) var exporting = false
    @Published var selected: Blakeswap_V1_ActivityRecord?
    let context: TradeContext
    private var snapshot = ""
    private var loadedFilters = ActivityFilters()
    private var requestID = UUID()
    private let call: (String, Data) async throws -> Data

    init(context: TradeContext, root: String, call: ((String, Data) async throws -> Data)? = nil) {
        self.context = context
        self.call = call ?? { method, payload in try await DaemonRPC.call(root: root, profile: context.profile, method: method, payload: payload) }
    }
    var isEmpty: Bool { phase == .loaded && records.isEmpty }
    func load(more: Bool = false, current: () -> TradeContext) async {
        guard context.matches(current()), !exporting else { return }
        if more && (phase == .loading || nextCursor == 0 || loadedFilters != filters) { return }
        let id = UUID(); requestID = id
        let scope = filters
        var query = scope.query(context: context)
        if more { query.snapshot = snapshot; query.cursor = nextCursor }
        else { records = []; selected = nil; snapshot = ""; nextCursor = 0; total = 0 }
        phase = .loading; error = nil
        do {
            let data = try await call("activity.list", query.jsonUTF8Data())
            let page = try Blakeswap_V1_ActivityPage(serializedBytes: data)
            guard !Task.isCancelled, requestID == id, scope == filters, context.matches(current()) else { return }
            guard !page.snapshot.isEmpty, (!more || page.snapshot == snapshot),
                  page.records.allSatisfy({ $0.wallet == context.profile && $0.network == context.network }) else { throw RPCError.message("Activity belongs to a different wallet or snapshot. Refresh history.") }
            if more { records += page.records } else { records = page.records }
            snapshot = page.snapshot; nextCursor = page.nextCursor; total = page.total; loadedFilters = scope
            indexing = page.index.keys.sorted().compactMap { chain in
                guard let index = page.index[chain] else { return nil }
                if !index.error.isEmpty { return "\(symbol(chain)): \(index.error)" }
                if index.completedPass == 0 { return "\(symbol(chain)): indexing historical receipts, including spent outputs." }
                return nil
            }
            if !page.error.isEmpty { indexing.append(page.error) }
            phase = .loaded
        } catch {
            guard !Task.isCancelled, requestID == id, scope == filters, context.matches(current()) else { return }
            self.error = error.localizedDescription; phase = .failed
        }
    }
    func export(current: () -> TradeContext) async -> String? {
        guard context.matches(current()), phase == .loaded, !snapshot.isEmpty, !exporting, filters == loadedFilters else { return nil }
        exporting = true; error = nil
        defer { exporting = false }
        let scope = loadedFilters, token = snapshot
        var query = scope.query(context: context); query.snapshot = token; query.limit = 500
        var csv = "", seen = Set<UInt32>()
        do {
            repeat {
                guard context.matches(current()), scope == filters, token == snapshot, !Task.isCancelled else { return nil }
                guard seen.insert(query.cursor).inserted else { throw RPCError.message("Activity export cursor did not advance. Refresh history.") }
                let data = try await call("activity.export", query.jsonUTF8Data())
                let chunk = try Blakeswap_V1_ActivityExport(serializedBytes: data)
                guard context.matches(current()), scope == filters, token == snapshot, !Task.isCancelled else { return nil }
                guard chunk.snapshot == token, chunk.total == total else { throw RPCError.message("Activity export snapshot changed. Refresh history.") }
                csv += chunk.csv
                query.cursor = chunk.nextCursor
            } while query.cursor != 0
            return csv
        } catch {
            if context.matches(current()) { self.error = error.localizedDescription }
            return nil
        }
    }
    func select(_ record: Blakeswap_V1_ActivityRecord) {
        guard record.wallet == context.profile, record.network == context.network else { return }
        selected = record
    }
}

extension Blakeswap_V1_ActivityRecord: Identifiable {}

func activityDate(_ value: Int64) -> String {
    value == 0 ? "Unknown" : Date(timeIntervalSince1970: TimeInterval(value)).formatted(date: .abbreviated, time: .standard)
}
func activitySortDate(_ record: Blakeswap_V1_ActivityRecord) -> Int64 {
    record.createdAt > 0 ? record.createdAt : (record.blockTime > 0 ? record.blockTime : record.recordedAt)
}

// Explorer configuration is explicit for both chain and network. Empty means no
// link; a Bitcoin URL is never substituted for a missing Blake explorer.
func activityExplorer(record: Blakeswap_V1_ActivityRecord, txid: String, settings: AppSettings?) -> URL? {
    guard ["btc", "blake"].contains(record.chain), ["mainnet", "testnet", "regtest"].contains(record.network),
          txid.utf8.count == 64, txid.utf8.allSatisfy({ (48...57).contains($0) || (65...70).contains($0) || (97...102).contains($0) }),
          let template = settings?.environments.first(where: { $0.network == record.network })?.explorers[record.chain],
          template.components(separatedBy: "{txid}").count == 2,
          let url = URL(string: template.replacingOccurrences(of: "{txid}", with: txid)), let host = url.host,
          url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
          url.scheme == "https" || (record.network == "regtest" && url.scheme == "http" && ["127.0.0.1", "::1", "[::1]"].contains(host)) else { return nil }
    return url
}

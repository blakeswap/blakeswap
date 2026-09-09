import Foundation
import SwiftProtobuf

struct MarketFilters: Equatable {
    var owner = "all"
    var side = "all"
    var status = "open"
    var minimum = ""
    var maximum = ""
    var sort = "rate"
    var descending = false
    var key: String { [owner, side, status, minimum, maximum, sort, String(descending)].joined(separator: "|") }
    func query(context: TradeContext) throws -> Blakeswap_V1_MarketQuery {
        let low = minimum.isEmpty ? 0 : Int64(minimum)
        let high = maximum.isEmpty ? 0 : Int64(maximum)
        guard let low, let high, (0...10_000_000_000).contains(low), (0...10_000_000_000).contains(high), high == 0 || high >= low else {
            throw RPCError.message("BTC amount filters need whole satoshis from 0 to 10 billion, with maximum at least minimum.")
        }
        var q = Blakeswap_V1_MarketQuery()
        q.expectedWallet = context.profile; q.expectedNetwork = context.network
        q.owner = owner; q.side = side; q.status = status; q.btcMin = low; q.btcMax = high
        q.sort = sort; q.descending = descending; q.limit = 100
        return q
    }
}

extension Blakeswap_V1_MarketOrder: Identifiable {
    var id: String { offer.bookID }
    var sideLabel: String { side == "buy_btc" ? "You buy BTC" : "You sell BTC" }
    var publicationLabel: String {
        switch publication {
        case "relay_acknowledged": return "Relay storage acknowledged"
        case "local_committed": return "Saved locally; relay publication pending"
        default: return "Prior relay publication unknown"
        }
    }
    var availabilityLabel: String {
        switch availability {
        case "stale": return "Stale relay view — refresh"
        case "publication_pending": return "Publication pending"
        case "pending": return "Your request is pending"
        case "reserved": return "Reserved for a swap"
        default: return status.capitalized
        }
    }
    var swapDestination: ActivityDestination? { swapIds.first.flatMap(ActivityDestination.swap) }
}

@MainActor
final class MarketModel: ObservableObject {
    @Published var filters = MarketFilters()
    @Published private(set) var rows: [Blakeswap_V1_MarketOrder] = []
    @Published private(set) var busy = false
    @Published private(set) var loaded = false
    @Published private(set) var error: String?
    @Published private(set) var more = false
    @Published private(set) var total: UInt32 = 0
    @Published private(set) var observedAt: Int64 = 0
    @Published private(set) var allRelays = false
    let context: TradeContext
    private var revision = ""
    private var offset: UInt32 = 0
    private var loadedFilters = MarketFilters()
    private var requestID = UUID()
    private var cancelling = false
    private let call: (String, Data) async throws -> Data

    init(context: TradeContext, root: String, call: ((String, Data) async throws -> Data)? = nil) {
        self.context = context
        self.call = call ?? { method, payload in try await DaemonRPC.call(root: root, profile: context.profile, method: method, payload: payload) }
    }
    var isEmpty: Bool { loaded && rows.isEmpty && error == nil }
    func load(more append: Bool = false, current: () -> TradeContext) async {
        guard !cancelling, context.matches(current()), !append || (!busy && more && loadedFilters == filters) else { return }
        let id = UUID(); requestID = id
        let scope = filters
        busy = true; error = nil
        if scope != loadedFilters { rows = []; loaded = false; more = false }
        defer { if id == requestID { busy = false } }
        do {
            var query = try scope.query(context: context)
            if append { query.revision = revision; query.offset = offset }
            let data = try await call("market.list", query.jsonUTF8Data())
            let page = try Blakeswap_V1_MarketPage(serializedBytes: data)
            guard !Task.isCancelled, id == requestID, scope == filters, context.matches(current()) else { return }
            guard page.wallet == context.profile, page.network == context.network, !page.revision.isEmpty,
                  !append || page.revision == revision,
                  page.records.allSatisfy({ $0.offer.network == context.network }) else { throw RPCError.message("Market scope changed. Refresh this wallet's orders.") }
            if append { rows += page.records } else { rows = page.records }
            revision = page.revision; offset = page.nextOffset; more = page.more; total = page.total
            observedAt = page.observedAt; allRelays = page.allRelays; loadedFilters = scope; loaded = true
        } catch {
            guard !Task.isCancelled, id == requestID, scope == filters, context.matches(current()) else { return }
            self.error = error.localizedDescription; more = false
        }
    }
    func refreshParent(_ previous: Order, current: () -> TradeContext) async throws -> Blakeswap_V1_MarketOrder {
        guard context.matches(current()) else { throw RPCError.message("Wallet changed.") }
        let query = try filters.query(context: context)
        let data = try await call("market.list", query.jsonUTF8Data())
        let page = try Blakeswap_V1_MarketPage(serializedBytes: data)
        guard !Task.isCancelled, context.matches(current()), page.wallet == context.profile,
              page.network == context.network, !page.revision.isEmpty else { throw RPCError.message("Market context changed.") }
        guard let row = page.records.first(where: { $0.offer.maker == previous.maker && $0.offer.id == previous.id }) else {
            throw RPCError.message("This parent is absent from the refreshed page. Your entered quantity is retained; reselect the order before submitting it.")
        }
        try validateRefreshedParent(previous, refreshed: row)
        return row
    }
    func cancel(_ order: Blakeswap_V1_MarketOrder, current: () -> TradeContext) async {
        guard !busy, order.own, order.canCancel, context.matches(current()) else { return }
        busy = true; cancelling = true; error = nil
        defer { cancelling = false; busy = false }
        var request = Blakeswap_V1_CancelOfferRequest()
        request.id = order.offer.id; request.expectedEventID = order.eventID
        request.expectedWallet = context.profile; request.expectedNetwork = context.network
        do {
            _ = try await call("offer.cancel", request.jsonUTF8Data())
            guard context.matches(current()) else { busy = false; return }
            busy = false; cancelling = false
            await load(current: current)
        } catch {
            busy = false
            if context.matches(current()) { self.error = error.localizedDescription }
        }
    }
}

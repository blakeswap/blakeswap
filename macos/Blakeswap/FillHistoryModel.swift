import Foundation
import SwiftProtobuf

typealias FillHistoryCall = (String, Data) async throws -> Data

struct ParentFillContext: Identifiable {
    let id = UUID()
    let wallet: TradeContext
    let maker: String
    let parentID: String
}

extension Blakeswap_V2_FillSummary {
    var allocationLabel: String {
        allocationKnown ? "\(disposition.capitalized): \(allocatedQuantity) sell sats currently allocated" : "Current maker allocation unknown"
    }
}

@MainActor
final class FillHistoryModel: ObservableObject {
    @Published private(set) var rows: [Blakeswap_V2_FillSummary] = []
    @Published private(set) var busy = false
    @Published private(set) var error: String?
    @Published private(set) var total: UInt32 = 0
    @Published private(set) var offset: UInt32 = 0
    @Published private(set) var more = false
    @Published private(set) var selected: Blakeswap_V2_RecordDetail?
    let context: ParentFillContext
    let limit: UInt32 = 100
    private var revision = ""
    private var nextOffset: UInt32 = 0
    private var attempt = UUID()
    private let call: FillHistoryCall

    init(context: ParentFillContext, call: @escaping FillHistoryCall) { self.context = context; self.call = call }
    func load(offset requested: UInt32 = 0, refresh: Bool = false, current: () -> TradeContext) async {
        guard context.wallet.matches(current()), !context.maker.isEmpty, !context.parentID.isEmpty,
              refresh || requested == 0 || !revision.isEmpty else { return }
        let id = UUID(); attempt = id; busy = true; error = nil; selected = nil
        defer { if id == attempt { busy = false } }
        let expected = refresh ? "" : revision
        var request = Blakeswap_V2_FillQuery()
        request.expectedWallet = context.wallet.profile; request.expectedNetwork = context.wallet.network
        request.parentMaker = context.maker; request.parentID = context.parentID
        request.offset = refresh ? 0 : requested; request.limit = limit; request.revision = expected
        do {
            let data = try await call("fills.list", request.jsonUTF8Data())
            let page = try Blakeswap_V2_FillPage(serializedBytes: data)
            guard !Task.isCancelled, id == attempt, context.wallet.matches(current()) else { return }
            let end = UInt64(request.offset) + UInt64(page.records.count)
            guard page.wallet == request.expectedWallet, page.network == request.expectedNetwork,
                  page.parentMaker == context.maker, page.parentID == context.parentID,
                  !page.revision.isEmpty, expected.isEmpty || (page.revision == expected && page.total == total),
                  page.records.count <= Int(limit), end <= UInt64(page.total),
                  page.nextOffset == UInt32(end), page.more == (end < UInt64(page.total)),
                  !page.more || !page.records.isEmpty,
                  Set(page.records.map(\.id)).count == page.records.count,
                  page.records.map(\.id) == page.records.map(\.id).sorted(),
                  page.records.allSatisfy({ !$0.id.isEmpty && $0.parentMaker == context.maker && $0.parentID == context.parentID }) else {
                throw RPCError.message("Fill history identity or frozen page changed. Refresh this parent explicitly.")
            }
            // One page only: visiting lifetime history never grows a native row array.
            rows = page.records; total = page.total; offset = request.offset
            revision = page.revision; nextOffset = page.nextOffset; more = page.more
        } catch {
            guard !Task.isCancelled, id == attempt, context.wallet.matches(current()) else { return }
            self.error = error.localizedDescription; more = false
        }
    }
    func next(current: () -> TradeContext) async { guard !busy, more else { return }; await load(offset: nextOffset, current: current) }
    func previous(current: () -> TradeContext) async { guard !busy, offset > 0 else { return }; await load(offset: offset > limit ? offset-limit : 0, current: current) }
    func select(_ row: Blakeswap_V2_FillSummary, current: () -> TradeContext) async {
        guard !busy, rows.contains(row), context.wallet.matches(current()) else { return }
        let id = UUID(); attempt = id; busy = true; error = nil; selected = nil
        defer { if id == attempt { busy = false } }
        var request = Blakeswap_V2_RecordQuery()
        request.kind = "swap"; request.id = row.id; request.expectedWallet = context.wallet.profile; request.expectedNetwork = context.wallet.network
        do {
            let data = try await call("record.get", request.jsonUTF8Data())
            let detail = try Blakeswap_V2_RecordDetail(serializedBytes: data)
            guard !Task.isCancelled, id == attempt, context.wallet.matches(current()) else { return }
            guard detail.kind == "swap", detail.id == row.id, detail.hasSwap,
                  detail.swap.id == row.id, detail.swap.parentID == context.parentID, detail.swap.parentMaker == context.maker else { throw RPCError.message("Child detail does not belong to this parent.") }
            selected = detail
        } catch {
            guard !Task.isCancelled, id == attempt, context.wallet.matches(current()) else { return }
            self.error = error.localizedDescription
        }
    }
}

import SwiftUI

@MainActor
struct FillHistoryView: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.dismiss) private var dismiss
    @StateObject private var history: FillHistoryModel
    let order: Blakeswap_V1_MarketOrder?
    init(context: ParentFillContext, order: Blakeswap_V1_MarketOrder? = nil, call: @escaping FillHistoryCall) {
        self.order = order
        _history = StateObject(wrappedValue: FillHistoryModel(context: context, call: call))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Parent order fills").font(.title2)
            Text("\(history.context.wallet.profile) · \(history.context.wallet.network)\nMaker \(history.context.maker)\nParent \(history.context.parentID)").font(.caption.monospaced()).textSelection(.enabled)
            if let order, order.own, order.hasQuantities {
                let q = order.quantities
                Text("Total \(q.total) · Available \(q.available) · Reserved \(q.reserved) · Committed \(q.committed) · Filled \(q.filled) · Released \(q.released) sell sats")
                Text("Parent accounting at selection; it is not recomputed from this child page.").font(.caption)
            } else { Text("Current maker accounting is not locally known.").font(.caption) }
            if let error = history.error { Text(error).foregroundStyle(.orange) }
            if history.busy { ProgressView() }
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    ForEach(history.rows, id: \.id) { row in
                        VStack(alignment: .leading) {
                            Button(row.id) { Task { await history.select(row, current: { app.tradeContext }) } }.font(.caption.monospaced())
                            Text("Original fill: \(row.quantity) sell / \(row.buyAmount) buy sats · Revision \(row.parentRevision)")
                            Text("\(row.stage) · \(row.allocationLabel)").font(.caption)
                            Text("\(row.archived ? "Archived" : "Active")\(row.monitoringRequired ? " · Monitoring required" : "")").font(.caption).foregroundStyle(row.monitoringRequired ? .orange : .secondary)
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading)
            }.frame(maxHeight: 340)
            if let detail = history.selected {
                Text("Child \(detail.id): \(detail.swap.stage)").font(.headline)
                Text("\(detail.message)\(detail.monitoringRequired ? " Monitoring remains required." : "")").font(.caption)
                Text("Long funding: \(detail.swap.long.txid)\nShort funding: \(detail.swap.short.txid)").font(.caption.monospaced()).textSelection(.enabled)
            }
            HStack {
                Button("Previous") { Task { await history.previous(current: { app.tradeContext }) } }.disabled(history.busy || history.offset == 0)
                Text("Page starts at \(history.offset) · \(history.total) local children").font(.caption)
                Button("Next") { Task { await history.next(current: { app.tradeContext }) } }.disabled(history.busy || !history.more)
                Spacer()
                Button("Refresh") { Task { await history.load(refresh: true, current: { app.tradeContext }) } }.disabled(history.busy)
                Button("Done") { dismiss() }
            }
        }.padding(24).frame(width: 740)
            .task { await history.load(current: { app.tradeContext }) }
    }
}

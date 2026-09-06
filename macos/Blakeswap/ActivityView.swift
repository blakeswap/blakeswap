import AppKit
import SwiftUI
import UniformTypeIdentifiers

@MainActor
struct ActivityView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var model: ActivityModel
    @State private var dateFilter = false
    @State private var from = Calendar.current.date(byAdding: .month, value: -1, to: .now) ?? .now
    @State private var to = Date()
    init(context: TradeContext, root: String) { _model = StateObject(wrappedValue: ActivityModel(context: context, root: root)) }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack {
                Text("Activity history").font(.title3.bold())
                Spacer()
                Button("Refresh") { Task { await model.load(current: { app.tradeContext }) } }.disabled(model.phase == .loading || model.exporting)
                Button(model.exporting ? "Exporting…" : "Export selected scope…") { export() }
                    .disabled(model.phase != .loaded || model.exporting).accessibilityIdentifier("export-activity")
            }
            Text("Amounts are native satoshis. Linked change and settlement receipts are informational; count the parent payment once. Confirmations can reverse after a reorg.").font(.caption).foregroundStyle(.secondary)
            HStack {
                Picker("Type", selection: $model.filters.kind) {
                    Text("All types").tag("")
                    ForEach(["receive", "send", "order", "swap", "swap_funding", "swap_claim", "swap_refund", "tower_earning"], id: \.self) { Text($0.replacingOccurrences(of: "_", with: " ").capitalized).tag($0) }
                }
                Picker("Status", selection: $model.filters.status) {
                    Text("All statuses").tag("")
                    ForEach(["prepared", "broadcast", "mempool", "confirming", "confirmed", "unknown", "unverified", "orphaned", "conflicted", "open", "cancelled", "expired", "filled", "completed", "refunded", "rejected"], id: \.self) { Text($0.capitalized).tag($0) }
                }
                Picker("Asset", selection: $model.filters.chain) { Text("All assets").tag(""); Text("BTC").tag("btc"); Text("BLAKE").tag("blake") }
            }.disabled(model.exporting)
            HStack {
                Toggle("Date range", isOn: $dateFilter)
                if dateFilter {
                    DatePicker("From", selection: $from, displayedComponents: .date)
                    DatePicker("Through", selection: $to, displayedComponents: .date)
                }
            }.disabled(model.exporting)
            Text("Sorted by known local creation time, otherwise block time, otherwise first indexing time. Unknown historical creation times stay unknown.").font(.caption).foregroundStyle(.secondary)
            ForEach(model.indexing, id: \.self) { Text($0).font(.caption).foregroundStyle(.orange) }
            if let error = model.error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
            if model.phase == .loading { ProgressView("Loading activity…").accessibilityIdentifier("activity-loading") }
            if model.phase == .failed { Button("Retry history") { Task { await model.load(current: { app.tradeContext }) } } }
            if model.isEmpty { ContentUnavailableView("No activity in this scope", systemImage: "clock", description: Text("Try another filter. Historical indexing continues while your wallet is open.")).accessibilityIdentifier("activity-empty") }
            LazyVStack(spacing: 10) {
                ForEach(model.records) { record in
                    Button { model.select(record) } label: {
                        HStack(alignment: .top) {
                            VStack(alignment: .leading, spacing: 5) {
                                Text(record.label).font(.headline)
                                Text("\(activityDate(activitySortDate(record))) · \(record.status)").font(.caption).foregroundStyle(.secondary)
                                Text(record.movement ? record.direction.capitalized : "Linked information · excluded from payment totals").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            VStack(alignment: .trailing, spacing: 5) {
                                Text("\(record.movement ? record.amount : record.principal) \(symbol(record.chain)) sats").monospacedDigit()
                                if record.feeKnown { Text("Fee: \(record.fee) sats\(record.feePayer == "contract_owner" ? " · contract owner pays" : "")").font(.caption).foregroundStyle(.secondary) }
                            }
                        }.padding(16).background(panel, in: RoundedRectangle(cornerRadius: 12))
                    }.buttonStyle(.plain).accessibilityIdentifier("activity-\(record.id)")
                }
            }
            HStack {
                Text("\(model.records.count) of \(model.total) entries in this snapshot").font(.caption).foregroundStyle(.secondary)
                Spacer()
                if model.nextCursor > 0 { Button("Load more") { Task { await model.load(more: true, current: { app.tradeContext }) } }.disabled(model.phase == .loading || model.exporting) }
            }
        }
        .task { await model.load(current: { app.tradeContext }) }
        .onChange(of: model.filters) { _, _ in Task { await model.load(current: { app.tradeContext }) } }
        .onChange(of: dateFilter) { _, _ in updateDates() }
        .onChange(of: from) { _, _ in updateDates() }
        .onChange(of: to) { _, _ in updateDates() }
        .sheet(item: $model.selected) { record in
            ActivityDetails(record: record, context: model.context).environmentObject(app)
        }
    }
    private func updateDates() {
        var filters = model.filters
        filters.from = dateFilter ? Int64(Calendar.current.startOfDay(for: from).timeIntervalSince1970) : 0
        filters.to = dateFilter ? Int64((Calendar.current.date(byAdding: .day, value: 1, to: Calendar.current.startOfDay(for: to)) ?? to).timeIntervalSince1970) - 1 : 0
        model.filters = filters
    }
    private func export() {
        let panel = NSSavePanel()
        panel.allowedContentTypes = [.commaSeparatedText]
        panel.nameFieldStringValue = "blakeswap-\(model.context.profile)-\(model.context.network)-activity.csv"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task {
            guard let csv = await model.export(current: { app.tradeContext }), model.context.matches(app.tradeContext) else { return }
            do { try Data(csv.utf8).write(to: url, options: .atomic); app.notice = "Activity scope exported to \(url.lastPathComponent)." }
            catch { app.notice = "Could not export activity: \(error.localizedDescription)" }
        }
    }
}

@MainActor
struct ActivityDetails: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.dismiss) private var dismiss
    let record: Blakeswap_V1_ActivityRecord
    let context: TradeContext
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(record.label).font(.title2.bold())
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    Text("\(record.wallet) · \(record.network) · \(symbol(record.chain))").foregroundStyle(.secondary)
                    Text("Status: \(record.status) · \(record.confirmations) confirmations")
                    Text("Amount: \(record.amount) native sats · Principal: \(record.principal) sats")
                    Text(record.movement ? "This is the \(record.direction) payment entry." : "This entry is linked information and is excluded from payment totals.")
                    if !record.classification.isEmpty { Text("Classification: \(record.classification.replacingOccurrences(of: "_", with: " "))").font(.caption) }
                    if record.feeKnown { Text("Actual known mining fee: \(record.fee) sats · Payer: \(record.feePayer.replacingOccurrences(of: "_", with: " "))") }
                    else { Text("Mining fee: unknown or not applicable to this record") }
                    if record.bounty > 0 { Text("Tower bounty paid: \(record.bounty) sats") }
                    if !record.counterChain.isEmpty { Text("Counter principal: \(record.counterAmount) \(symbol(record.counterChain)) sats") }
                    Text("Local creation: \(activityDate(record.createdAt)) · Provenance: \(record.createdSource)")
                    Text("First recorded locally: \(activityDate(record.recordedAt))")
                    Text("Block time: \(activityDate(record.blockTime)) · Latest observation: \(activityDate(record.observedAt))")
                    Text("Observation source: \(record.source.isEmpty ? "Unknown" : record.source) · Generation \(record.generation)").font(.caption.monospaced())
                    Text("ID: \(record.id)\nGroup: \(record.groupID)").font(.caption.monospaced())
                    if !record.address.isEmpty { Text("Address: \(record.address)").font(.caption.monospaced()) }
                    HStack {
                        if let target = ActivityDestination.order(record.orderID) { Button("Show order") { navigate(target) } }
                        if let target = ActivityDestination.swap(record.swapID) { Button("Show swap") { navigate(target) } }
                        if let target = ActivityDestination.send(record.sendID) { Button("Show send") { navigate(target) } }
                    }.disabled(!context.matches(app.tradeContext))
                    if !record.variants.isEmpty {
                        Divider(); Text("Transaction lineage").font(.headline)
                        ForEach(record.variants, id: \.self) { txid in
                            VStack(alignment: .leading, spacing: 4) {
                                Text(txid).font(.caption.monospaced())
                                if let amounts = record.variantAmounts.first(where: { $0.txid == txid }) { Text("Amount \(amounts.amount) sats · Fee \(amounts.fee) sats").font(.caption) }
                                if let url = activityExplorer(record: record, txid: txid, settings: app.settings) { Link("Open \(symbol(record.chain)) \(record.network) explorer", destination: url).font(.caption) }
                            }
                        }
                        if activityExplorer(record: record, txid: record.txid, settings: app.settings) == nil { Text("No explorer configured for this asset and network. Add a transaction URL template in Settings.").font(.caption).foregroundStyle(.secondary) }
                    }
                    ForEach(record.outpoints.indices, id: \.self) { i in Text("Outpoint: \(record.outpoints[i].txid):\(record.outpoints[i].vout)").font(.caption.monospaced()) }
                    if !record.history.isEmpty {
                        Divider(); Text("Prior observed outcomes").font(.headline)
                        Text("These outcomes are retained for audit; they are not the current final status.").font(.caption).foregroundStyle(.secondary)
                        ForEach(record.history.indices.reversed(), id: \.self) { i in
                            let outcome = record.history[i]
                            Text("\(activityDate(outcome.observedAt)) · \(outcome.status) · \(outcome.amount) sats · \(outcome.txid)").font(.caption.monospaced())
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
            }.frame(maxHeight: 560)
            Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
        }.padding(28).frame(width: 660)
    }
    private func navigate(_ destination: ActivityDestination) {
        guard context.matches(app.tradeContext) else { return }
        app.activityDestination = destination; app.page = destination.page; dismiss()
    }
}

import SwiftUI

struct ManageOfferContext: Identifiable {
    let id = UUID()
    let action: String
    let order: Blakeswap_V2_MarketOrder
    let wallet: TradeContext
}

@MainActor
struct MarketView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var market: MarketModel
    @State private var taking: TakeOfferContext?
    @State private var managing: ManageOfferContext?
    @State private var selected: Blakeswap_V2_MarketOrder?
    @State private var focusedID = ""
    @State private var fillParent: Blakeswap_V2_MarketOrder?
    private let fillCall: FillHistoryCall?
    let context: TradeContext
    let root: String

    init(context: TradeContext, root: String, fillCall: FillHistoryCall? = nil) {
        self.context = context; self.root = root; self.fillCall = fillCall
        _market = StateObject(wrappedValue: MarketModel(context: context, root: root))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            filters
            HStack {
                Text("Rates: BLAKE per 1 BTC (display rounded; comparisons exact)").font(.caption)
                Spacer()
                Button("Refresh market") { Task { await app.refresh(); await market.load(current: { app.tradeContext }) } }.disabled(market.busy)
            }
            if market.observedAt == 0 {
                Text("Relay availability is not verified yet. Your saved orders remain available below.").font(.caption).foregroundStyle(.orange)
            } else {
                Text("Relay view: \(Date(timeIntervalSince1970: TimeInterval(market.observedAt)).formatted(date: .omitted, time: .standard))\(market.allRelays ? "" : " · Some configured relays unavailable"). The maker decides availability at acceptance.").font(.caption).foregroundStyle(.secondary)
            }
            if let error = market.error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
            if market.busy { ProgressView("Reading market…") }
            if market.isEmpty { ContentUnavailableView("No matching orders", systemImage: "line.3.horizontal.decrease.circle").frame(maxWidth: .infinity).padding() }
            ForEach(market.rows) { row in
                HStack(alignment: .top, spacing: 18) {
                    VStack(alignment: .leading, spacing: 5) {
                        Text(row.sideLabel).font(.headline)
                        Text("\(units(row.btcAmount)) BTC ↔ \(units(row.blakeAmount)) BLAKE").font(.body.monospaced())
                        Text("≈ \(row.rate) BLAKE / BTC").font(.caption.monospaced())
                        Text(row.own ? "Your order" : "Maker \(row.offer.maker.prefix(12))…").font(.caption).foregroundStyle(.secondary)
                        Text("Expires \(Date(timeIntervalSince1970: TimeInterval(row.offer.expires)).formatted())").font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    VStack(alignment: .trailing, spacing: 8) {
                        Text(row.availabilityLabel).font(.callout)
                        if row.own { Text(row.publicationLabel).font(.caption).foregroundStyle(.secondary) }
                        Button("Details") { selected = row }
                        actions(row)
                    }
                }.padding(18).background(panel, in: RoundedRectangle(cornerRadius: 12)).id("order/" + row.offer.id)
            }
            HStack {
                Text("\(market.rows.count) of \(market.total) matching orders").font(.caption).foregroundStyle(.secondary)
                Spacer()
                if market.more { Button("Load more") { Task { await market.load(more: true, current: { app.tradeContext }) } }.disabled(market.busy) }
            }
        }
        .task(id: market.filters.key + "|" + focusedID) {
            await market.load(current: { app.tradeContext })
            if !focusedID.isEmpty {
                var pages = 0
                while !Task.isCancelled, market.more, !market.rows.contains(where: { $0.offer.id == focusedID }), pages < 20 {
                    pages += 1; await market.load(more: true, current: { app.tradeContext })
                }
                selected = market.rows.first { $0.offer.id == focusedID }
            }
        }
        .task {
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(15)) } catch { break }
                if !market.busy, taking == nil, managing == nil, selected == nil { await market.load(current: { app.tradeContext }) }
            }
        }
        .task(id: app.activityDestination?.anchor ?? "") {
            if let target = app.activityDestination, target.page == "Market", target.anchor.hasPrefix("order/") {
                market.filters = MarketFilters(owner: "mine", status: "all")
                focusedID = String(target.anchor.dropFirst(6))
            }
        }
        .sheet(item: $taking, onDismiss: { Task { await market.load(current: { app.tradeContext }) } }) { item in TradeComposer(context: item.wallet, root: root, order: item.order, suggestedQuantity: item.suggestedQuantity, expectedEventID: item.eventID, refreshParent: { order in try await market.refreshParent(order, current: { app.tradeContext }) }).environmentObject(app) }
        .sheet(item: $managing, onDismiss: { Task { await market.load(current: { app.tradeContext }) } }) { item in TradeComposer(context: item.wallet, root: root, management: item).environmentObject(app) }
        .sheet(item: $fillParent) { row in
            if let fillCall {
                FillHistoryView(context: ParentFillContext(wallet: context, maker: row.offer.maker, parentID: row.offer.id), order: row, call: fillCall).environmentObject(app)
            }
        }
        .sheet(item: $selected) { row in
            VStack(alignment: .leading, spacing: 14) {
                Text(row.sideLabel).font(.title2)
                Text("Order \(row.offer.id)").font(.caption.monospaced()).textSelection(.enabled)
                Text(row.availabilityLabel)
                Text("\(row.btcAmount) BTC sats · \(row.blakeAmount) BLAKE sats")
                Text("Rate ≈ \(row.rate) BLAKE / BTC")
                Text("Expires \(Date(timeIntervalSince1970: TimeInterval(row.offer.expires)).formatted())")
                if row.own { Text(row.publicationLabel) }
                if row.createdAt > 0 { Text("Created \(Date(timeIntervalSince1970: TimeInterval(row.createdAt)).formatted())") }
                else { Text("Original creation time unknown").foregroundStyle(.secondary) }
                if !row.replaces.isEmpty { Text("Replaces \(row.replaces)").font(.caption.monospaced()).textSelection(.enabled) }
                if !row.replacedBy.isEmpty { Text("Replaced by \(row.replacedBy)").font(.caption.monospaced()).textSelection(.enabled) }
                if !row.recreatedFrom.isEmpty { Text("Recreated from \(row.recreatedFrom)").font(.caption.monospaced()).textSelection(.enabled) }
                ForEach(row.swapIds, id: \.self) { id in
                    Button("Show swap \(id.prefix(12))…") { selected = nil; app.activityDestination = .swap(id); app.page = "Swaps" }.accessibilityIdentifier("order-swap-\(id)")
                }
                Text("Cancellation withdraws only the available remainder. Accepted children keep settling. Relay acknowledgement records publication; it does not remove child obligations.").font(.caption).foregroundStyle(.secondary)
                Button("Done") { selected = nil }
            }.padding(28).frame(width: 620)
        }
    }
    private var filters: some View {
        VStack(spacing: 10) {
            HStack {
                Picker("Owner", selection: $market.filters.owner) { Text("All makers").tag("all"); Text("My orders").tag("mine"); Text("Other makers").tag("others") }
                Picker("Your side", selection: $market.filters.side) { Text("Both directions").tag("all"); Text("Buy BTC").tag("buy_btc"); Text("Sell BTC").tag("sell_btc") }
                Picker("Status", selection: $market.filters.status) {
                    ForEach(["all", "open", "pending", "reserved", "filled", "cancelled", "expired", "refunded"], id: \.self) { Text($0.capitalized).tag($0) }
                }
            }
            HStack {
                TextField("Minimum BTC sats", text: $market.filters.minimum)
                TextField("Maximum BTC sats", text: $market.filters.maximum)
                Picker("Sort", selection: $market.filters.sort) { Text("Rate").tag("rate"); Text("BTC size").tag("size"); Text("Expiry").tag("expiry") }
                Toggle("Descending", isOn: $market.filters.descending)
            }.textFieldStyle(.roundedBorder)
        }.accessibilityIdentifier("market-filters")
    }
    @ViewBuilder private func actions(_ row: Blakeswap_V2_MarketOrder) -> some View {
        HStack {
            if fillCall != nil { Button("Fill history") { fillParent = row } }
            if row.canTake { Button("Take offer") { taking = TakeOfferContext(order: row.offer, wallet: context, suggestedQuantity: row.suggestedQuantity, eventID: row.eventID) }.accessibilityIdentifier("take-offer-\(row.offer.id)") }
            if row.canCancel { Button("Cancel") { Task { await market.cancel(row, current: { app.tradeContext }) } } }
            if row.canReplace { Button("Edit / replace") { managing = ManageOfferContext(action: "replace", order: row, wallet: context) } }
            if row.canRecreate { Button("Recreate") { managing = ManageOfferContext(action: "recreate", order: row, wallet: context) } }
        }.disabled(market.busy || !context.matches(app.tradeContext))
    }
}

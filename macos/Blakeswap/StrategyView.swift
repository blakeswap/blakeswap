import SwiftUI

@MainActor
struct StrategyView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var model: StrategyModel
    @State private var draft: StrategyDraft?
    let context: TradeContext
    let root: String
    init(context: TradeContext, root: String) {
        self.context = context; self.root = root
        _model = StateObject(wrappedValue: StrategyModel(context: context, root: root))
    }
    var body: some View {
        DisclosureGroup("Inventory-aware market making · opt-in") {
            VStack(alignment: .leading, spacing: 14) {
                Text("Offer both assets through one reviewed strategy. Confirmed inventory, whole-input reserves and shared fee limits control each quote. Runs only while this wallet’s daemon is running.").font(.caption).foregroundStyle(.secondary)
                HStack {
                    if model.strategies.isEmpty { Button("Create strategy") { draft = StrategyDraft() } }
                    Button("Refresh inventory and decisions") { Task { await model.load(current: { app.tradeContext }) } }
                    if model.busy { ProgressView().controlSize(.small) }
                }.disabled(model.busy || !context.matches(app.tradeContext))
                if let error = model.error { Text(error).foregroundStyle(.orange) }
                if model.loaded && model.strategies.isEmpty { Text("No strategy. Inventory-aware trading is off.").foregroundStyle(.secondary) }
                ForEach(model.strategies) { p in
                    VStack(alignment: .leading, spacing: 10) {
                        Text(p.restoreHold ? "Imported authorization held" : p.tripped ? "Circuit breaker paused trading" : p.enabled ? "Enabled" : "Paused / stopped").font(.headline)
                        Text(p.decision).foregroundStyle(.secondary)
                        Text("\(p.activeQuotes) wallet quotes · \(p.activeSwaps) unsettled swaps · consecutive failures \(p.consecutiveFailures), replacement failures \(p.replacementFailures)").font(.caption)
                        StrategyInventoryView(strategy: p)
                        StrategyPreviewView(quotes: p.quotes)
                        HStack {
                            Button("Read confirmed activity report") { Task { await model.report(p, current: { app.tradeContext }) } }
                            Button("Edit / review limits") { draft = StrategyDraft(p) }
                            if p.enabled {
                                Button("Pause and cancel open quotes") { Task { await model.stop(p, permanent: false, current: { app.tradeContext }) } }
                                Button("Stop strategy") { Task { await model.stop(p, permanent: true, current: { app.tradeContext }) } }
                            }
                        }.disabled(model.busy || !context.matches(app.tradeContext))
                        Text("Pause and stop revoke new quotes and unsigned retries. Accepted trades continue settling or refunding. Resuming requires reviewing current limits.").font(.caption).foregroundStyle(.secondary)
                    }.padding().background(panel, in: RoundedRectangle(cornerRadius: 12))
                }
            }.padding(.top, 12)
        }
        .task { await model.load(current: { app.tradeContext }) }
        .task {
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(15)) } catch { break }
                if draft == nil { await model.load(current: { app.tradeContext }) }
            }
        }
        .sheet(item: $draft, onDismiss: { Task { await model.load(current: { app.tradeContext }) } }) { d in
            StrategyEditor(context: context, root: root, draft: d).environmentObject(app)
        }
    }
}

struct StrategyInventoryView: View {
    let strategy: Blakeswap_V1_StrategyView
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(["btc", "blake"], id: \.self) { id in
                if let i = strategy.inventory[id] {
                    VStack(alignment: .leading, spacing: 3) {
                        Text("\(id.uppercased()) · \(i.fresh ? "Current confirmed inventory" : "Inventory observation unavailable")").font(.subheadline.bold())
                        Text("Unlocked \(i.funds.unlockedConfirmed) · reserved \(i.funds.reservedConfirmed) · pending \(i.funds.unconfirmed) · HTLC locked \(i.funds.htlcAvailable ? String(i.funds.htlcLocked) : "unknown") sats")
                        Text("Wallet exposure \(i.exposure) sats. Gross authorization: \(i.committedVolume) committed + \(i.reservedVolume) reserved sats.")
                        Text("Shared fee/rescue authorization: \(i.committedFees) committed + \(i.reservedFees) reserved sats.")
                        if strategy.reportIncluded { Text("Currently confirmed completed volume \(i.confirmedVolume) sats · known confirmed fees \(i.knownFees) sats · rescue bounties \(i.knownBounties) sats · \(i.unknownFees) outcomes with unknown fee.") }
                    }.font(.caption).foregroundStyle(.secondary)
                }
            }
            Text("Report uses current confirmed activity evidence. Unverified outcomes are excluded; these totals are not profit or spendable funds. Gross authorization remains consumed even when evidence becomes unavailable.").font(.caption).foregroundStyle(.secondary)
        }
    }
}
struct StrategyPreviewView: View {
    let quotes: [Blakeswap_V1_StrategyQuote]
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Deterministic preview · checked again before publication").font(.subheadline.bold())
            ForEach(quotes, id: \.sell) { q in
                VStack(alignment: .leading, spacing: 3) {
                    Text("Sell \(q.sellAmount) \(q.sell.uppercased()) sats → receive \(q.buyAmount) \(q.sell == "btc" ? "BLAKE" : "BTC") sats").font(.caption.bold())
                    Text(q.reason).font(.caption).foregroundStyle(q.ready ? Color.secondary : Color.orange)
                    if q.ready {
                        Text("Exact BLAKE/BTC \(q.rate.numerator)/\(q.rate.denominator). Worst-case allowance: \(q.btcFees) BTC sats + \(q.blakeFees) BLAKE sats.").font(.caption)
                    }
                    if !q.referenceEvents.isEmpty { Text("Signed reference events: \(q.referenceEvents.joined(separator: ", "))").font(.caption.monospaced()).textSelection(.enabled) }
                }
            }
        }
    }
}

@MainActor
struct StrategyEditor: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var draft: StrategyDraft
    @StateObject private var model: StrategyModel
    let context: TradeContext
    init(context: TradeContext, root: String, draft: StrategyDraft) {
        self.context = context; _draft = State(initialValue: draft)
        _model = StateObject(wrappedValue: StrategyModel(context: context, root: root))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(model.review == nil ? "Two-asset maker strategy" : "Review full market-making authorization").font(.title2)
            Text("\(context.profile) · \(context.network)").font(.caption)
            ScrollView {
                if let r = model.review { authorization(r) }
                else { fields }
            }
            if let error = model.error { Text(error).foregroundStyle(.orange) }
            if !context.matches(app.tradeContext) { Text("Wallet or network changed; reopen this authorization.").foregroundStyle(.orange) }
            HStack {
                Button("Close") { dismiss() }; Spacer()
                if model.busy { ProgressView().controlSize(.small) }
                if model.review != nil {
                    Button("Back to edit") { model.revise() }
                    Button(draft.common.enabled ? "Authorize both directions" : "Save paused") { Task { if await model.save(current: { app.tradeContext }) { dismiss() } } }
                } else { Button("Review limits and preview") { Task { await model.review(draft, current: { app.tradeContext }) } } }
            }.disabled(model.busy || !context.matches(app.tradeContext))
        }.padding(24).frame(width: 800, height: 780)
    }
    private var fields: some View {
        VStack(alignment: .leading, spacing: 16) {
            side("BTC", $draft.btc); side("BLAKE", $draft.blake)
            GroupBox("Exact BLAKE-per-BTC reference and spreads") {
                VStack {
                    fraction("Fixed reference", $draft.common.rateN, $draft.common.rateD)
                    fraction("Hard minimum rate", $draft.common.minN, $draft.common.minD)
                    fraction("Hard maximum rate", $draft.common.maxN, $draft.common.maxD)
                    field("Base half-spread (bps)", $draft.spread); field("Minimum half-spread (bps)", $draft.minSpread)
                    field("Maximum half-spread (bps)", $draft.maxSpread); field("Maximum inventory skew (bps; 0 off)", $draft.skew)
                    Picker("Price reference", selection: $draft.common.reference) { Text("Explicit fixed reference").tag("fixed"); Text("Chosen external makers").tag("orderbook") }
                    if draft.common.reference == "orderbook" {
                        Text("3–16 external maker hex keys. Every identity must publish a fresh quote on each side. All configured relays must be current. Makers may collude.").font(.caption)
                        TextEditor(text: $draft.common.makers).font(.caption.monospaced()).frame(height: 80)
                        field("Reference freshness (30–300 seconds)", $draft.common.freshness); field("Maximum maker disagreement (1–100 bps)", $draft.common.spread)
                    }
                }
            }
            GroupBox("Shared execution, fees and circuit breakers") {
                VStack {
                    field("Check cadence (60–86400 seconds)", $draft.common.cadence); field("Offer lifetime (seconds, up to 7 days)", $draft.common.lifetime)
                    field("Wallet quotes + unsettled swaps (1–8)", $draft.concurrent)
                    field("Shared lifetime BTC fee/rescue budget", $draft.common.btcBudget); field("Shared lifetime BLAKE fee/rescue budget", $draft.common.blakeBudget)
                    field("Consecutive failed checks (1–20)", $draft.failures); field("Failed replacements (1–20)", $draft.replacements)
                    field("Failure rate breaker (bps; after 5, last 20 attempts)", $draft.failureRate)
                    field("Maximum rescue rate (0 off; up to 1000 bps)", $draft.common.towerBPS); field("Discovered provider hex key (empty when off)", $draft.common.towerKey)
                }
            }
            Toggle("Enable both maker directions after review", isOn: $draft.common.enabled)
            if draft.common.restored {
                Text("An imported snapshot can omit later spending. Existing uncertain reservations remain charged; recovery alone cannot enable the strategy.").foregroundStyle(.orange)
                Toggle("I authorize remaining limits despite potentially omitted later spending", isOn: $draft.common.acknowledgeRestored)
            }
        }.textFieldStyle(.roundedBorder).disabled(model.busy)
    }
    private func side(_ name: String, _ d: Binding<StrategySideDraft>) -> some View {
        GroupBox("\(name) limits · all amounts in \(name) satoshis") {
            VStack {
                field("Target confirmed inventory", d.target); field("Minimum unlocked reserve", d.reserve)
                field("Maximum wallet directional exposure", d.exposure); field("Minimum whole offer", d.minimum); field("Maximum whole offer", d.maximum)
                field("Lifetime gross sell authorization", d.volume); field("Fixed funding fee (0 = fresh estimate)", d.funding); field("Maximum funding fee", d.fundingCap)
            }
        }
    }
    private func field(_ label: String, _ text: Binding<String>) -> some View {
        HStack { Text(label).frame(maxWidth: .infinity, alignment: .leading); TextField(label, text: text).frame(width: 230) }
    }
    private func fraction(_ label: String, _ n: Binding<String>, _ d: Binding<String>) -> some View {
        HStack { Text(label).frame(maxWidth: .infinity, alignment: .leading); TextField("BLAKE sats", text: n); Text("/"); TextField("BTC sats", text: d) }
    }
    private func authorization(_ r: Blakeswap_V1_StrategyReview) -> some View {
        let c = r.config
        return VStack(alignment: .leading, spacing: 12) {
            Text(r.enabled ? "Enable both maker directions" : "Save paused authorization").font(.headline)
            Text("Strategy \(c.id) · revision \(r.expectedRevision)").font(.caption.monospaced())
            ForEach(["BTC", "BLAKE"], id: \.self) { id in
                let s = id == "BTC" ? c.btc : c.blake
                Text("\(id): target \(s.target), unlocked reserve \(s.minimumReserve), wallet exposure cap \(s.maxExposure), offer size \(s.minOffer)–\(s.maxOffer), lifetime gross sell \(s.volumeLimit) sats. Funding \(s.fundingFee == 0 ? "fresh estimate" : String(s.fundingFee)), cap \(s.maxFundingFee) sats.")
            }
            Text("BLAKE/BTC reference \(c.rate.numerator)/\(c.rate.denominator), hard bounds \(c.minRate.numerator)/\(c.minRate.denominator)–\(c.maxRate.numerator)/\(c.maxRate.denominator). Half-spread \(c.spreadBps) bps within \(c.minSpreadBps)–\(c.maxSpreadBps), skew at most \(c.skewBps) bps.")
            Text("Reference \(c.reference); freshness \(c.referenceFreshness)s; maximum disagreement \(c.referenceSpreadBps) bps.")
            if !c.referenceMakers.isEmpty { Text(c.referenceMakers.joined(separator: "\n")).font(.caption.monospaced()) }
            Text("Cadence \(c.cadence)s, lifetime \(c.lifetime)s, wallet quotes/unsettled swaps cap \(c.maxConcurrent). Shared lifetime fee/rescue budgets: \(c.btcFeeBudget) BTC sats and \(c.blakeFeeBudget) BLAKE sats.")
            Text("Rescue maximum \(c.towerBps) bps; provider \(c.towerPubkey.isEmpty ? "off" : c.towerPubkey). Breakers: \(c.maxConsecutiveFailures) consecutive failures, \(c.maxReplacementFailures) replacements, \(c.failureRateBps) bps failures after at least 5 of the last 20 attempts.")
            StrategyInventoryView(strategy: r.preview); StrategyPreviewView(quotes: r.preview.quotes)
            Text(r.warning).foregroundStyle(.orange)
        }.textSelection(.enabled)
    }
}

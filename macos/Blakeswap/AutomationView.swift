import SwiftUI

@MainActor
struct AutomationView: View {
    @EnvironmentObject private var app: AppModel
    @StateObject private var model: AutomationModel
    @State private var editing: AutomationDraft?
    let context: TradeContext
    let root: String
    init(context: TradeContext, root: String) {
        self.context = context; self.root = root
        _model = StateObject(wrappedValue: AutomationModel(context: context, root: root))
    }
    var body: some View {
        DisclosureGroup("Automatic offers · opt-in") {
            VStack(alignment: .leading, spacing: 14) {
                Text("Runs only while this wallet’s daemon is running. Each policy authorizes fixed-size offers within exact price, volume, fee and rescue limits. Disabling stops future offers; funded swaps continue.").font(.caption).foregroundStyle(.secondary)
                HStack {
                    Button("Create policy") { editing = AutomationDraft() }
                    Button("Refresh policies") { Task { await model.load(current: { app.tradeContext }) } }
                    if model.busy { ProgressView().controlSize(.small) }
                }.disabled(model.busy || !context.matches(app.tradeContext))
                if let error = model.error { Text(error).foregroundStyle(.orange) }
                if model.loaded && model.policies.isEmpty { Text("No policies. Automatic offers are off.").foregroundStyle(.secondary) }
                ForEach(model.policies.filter { $0.config.strategyID.isEmpty }) { p in
                    VStack(alignment: .leading, spacing: 8) {
                        Text("\(p.enabled ? "Enabled" : "Disabled") · Sell \(p.config.sellAmount) \(p.config.sell.uppercased()) sats · \(p.config.reference == "fixed" ? "Fixed rate" : "Selected maker reference")").font(.headline)
                        Text(p.decision).foregroundStyle(p.restoreHold ? .orange : .secondary)
                        Text("Policy \(p.id)").font(.caption.monospaced()).textSelection(.enabled)
                        if p.nextAction > 0 { Text("Next eligible check: \(Date(timeIntervalSince1970: TimeInterval(p.nextAction)).formatted())").font(.caption) }
                        Text("Sell volume: \(p.usage.committedVolume) committed + \(p.usage.reservedVolume) reserved / \(p.config.volumeLimit) sats")
                        Text("BTC fees/rescue: \(p.usage.committedBtcFees) committed + \(p.usage.reservedBtcFees) reserved / \(p.config.btcFeeBudget) sats").font(.caption)
                        Text("BLAKE fees/rescue: \(p.usage.committedBlakeFees) committed + \(p.usage.reservedBlakeFees) reserved / \(p.config.blakeFeeBudget) sats").font(.caption)
                        if !p.currentOfferID.isEmpty {
                            Button("Show current order · \(p.publication.replacingOccurrences(of: "_", with: " "))") { app.activityDestination = ActivityDestination(page: "Market", anchor: "order/" + p.currentOfferID) }
                        }
                        if !p.referenceEvents.isEmpty { Text("Reference signed events: \(p.referenceEvents.joined(separator: ", "))").font(.caption.monospaced()).textSelection(.enabled) }
                        HStack {
                            Button("Edit / review authorization") { editing = AutomationDraft(p) }
                            if p.enabled {
                                Button("Disable; keep open offers") { Task { await model.disable(p, cancelOpen: false, current: { app.tradeContext }) } }
                                Button("Disable and cancel open offers") { Task { await model.disable(p, cancelOpen: true, current: { app.tradeContext }) } }
                            }
                        }.disabled(model.busy || !context.matches(app.tradeContext))
                    }.padding().background(panel, in: RoundedRectangle(cornerRadius: 12))
                }
            }.padding(.top, 12)
        }
        .task { await model.load(current: { app.tradeContext }) }
        .task {
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(15)) } catch { break }
                if editing == nil { await model.load(current: { app.tradeContext }) }
            }
        }
        .sheet(item: $editing, onDismiss: { Task { await model.load(current: { app.tradeContext }) } }) { draft in
            AutomationEditor(context: context, root: root, draft: draft).environmentObject(app)
        }
    }
}

@MainActor
struct AutomationEditor: View {
    @EnvironmentObject private var app: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var draft: AutomationDraft
    @StateObject private var model: AutomationModel
    let context: TradeContext
    init(context: TradeContext, root: String, draft: AutomationDraft) {
        self.context = context; _draft = State(initialValue: draft)
        _model = StateObject(wrappedValue: AutomationModel(context: context, root: root))
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(model.review == nil ? "Automatic offer policy" : "Review automatic trading authorization").font(.title2)
            Text("\(context.profile) · \(context.network)").font(.caption)
            ScrollView {
                if let review = model.review { authorization(review) }
                else { fields }
            }
            if let error = model.error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
            if !context.matches(app.tradeContext) { Text("Wallet or network changed. Reopen the policy.").foregroundStyle(.orange) }
            HStack {
                Button("Close") { dismiss() }
                Spacer()
                if model.busy { ProgressView().controlSize(.small) }
                if model.review != nil {
                    Button("Back to edit") { model.revise() }.disabled(model.busy)
                    Button(draft.enabled ? "Authorize and enable" : "Save disabled policy") { Task { if await model.save(current: { app.tradeContext }) { dismiss() } } }
                        .disabled(model.busy || !context.matches(app.tradeContext)).accessibilityIdentifier("authorize-automation")
                } else {
                    Button("Review full authorization") { Task { await model.review(draft, current: { app.tradeContext }) } }
                        .disabled(model.busy || !context.matches(app.tradeContext))
                }
            }
        }.padding(24).frame(width: 760, height: 760)
    }
    private var fields: some View {
        VStack(alignment: .leading, spacing: 18) {
            GroupBox("Direction, size and lifetime sell-volume cap") {
                VStack {
                    Picker("Sell asset", selection: $draft.sell) { Text("BTC").tag("btc"); Text("BLAKE").tag("blake") }.disabled(draft.revision > 0)
                    field("Per-offer sell satoshis", $draft.size); field("Total gross sell satoshis", $draft.volume)
                    Text("Committed volume is never replenished by a refund. Accepted offers reserve capacity until conclusively unfunded or charged by signed funding.").font(.caption).foregroundStyle(.secondary)
                }
            }
            GroupBox("Exact BLAKE-per-BTC prices") {
                VStack {
                    fraction("Fixed rate", $draft.rateN, $draft.rateD); fraction("Hard minimum", $draft.minN, $draft.minD); fraction("Hard maximum", $draft.maxN, $draft.maxD)
                    Picker("Reference", selection: $draft.reference) { Text("Fixed rate renewal").tag("fixed"); Text("Chosen orderbook makers").tag("orderbook") }
                    if draft.reference == "orderbook" {
                        Text("3–16 external maker public keys, one per line. All must provide fresh same-side quotes. Makers may collude; this is not an independent price oracle.").font(.caption)
                        TextEditor(text: $draft.makers).frame(height: 80).font(.caption.monospaced())
                        field("Quote and relay freshness (30–300 seconds)", $draft.freshness); field("Maximum maker spread (1–100 bps)", $draft.spread)
                    }
                }
            }
            GroupBox("Schedule and fees") {
                VStack {
                    field("Offer lifetime (seconds, up to 7 days)", $draft.lifetime); field("Minimum check cadence (60–86400 seconds)", $draft.cadence); field("Maximum outstanding offers (1–8)", $draft.maxOpen)
                    field("Fixed funding fee (0 = fresh estimate)", $draft.funding); field("Maximum funding fee (sell-chain sats)", $draft.fundingCap)
                    field("Lifetime BTC fee / rescue cap (BTC sats)", $draft.btcBudget); field("Lifetime BLAKE fee / rescue cap (BLAKE sats)", $draft.blakeBudget)
                    Text("Each offer reserves the funding fee plus up to 20,000 settlement sats per chain and the selected rescue bounty. No offline catch-up burst.").font(.caption).foregroundStyle(.secondary)
                }
            }
            GroupBox("Watchtower authorization") {
                VStack {
                    field("Maximum rescue rate (0 off; 1–1000 bps)", $draft.towerBPS)
                    field("Discovered provider hex public key (empty when off)", $draft.towerKey)
                    Text("Every action requires a fresh valid provider proof within the selected rescue rate. No upfront charge. Accepted swaps retain their original proof and terms.").font(.caption).foregroundStyle(.secondary)
                }
            }
            Toggle("Enable future automatic offers after review", isOn: $draft.enabled)
            if draft.restored {
                Text("This imported policy may omit spending after its backup. Recovery readiness does not restore automatic spending authorization. Imported uncertain reservations remain charged even after enabling.").foregroundStyle(.orange)
                Toggle("I authorize the displayed remaining limits despite potentially missing later spending", isOn: $draft.acknowledgeRestored)
            }
        }.textFieldStyle(.roundedBorder).disabled(model.busy)
    }
    private func field(_ label: String, _ text: Binding<String>) -> some View {
        HStack { Text(label).frame(maxWidth: .infinity, alignment: .leading); TextField(label, text: text).frame(width: 260) }
    }
    private func fraction(_ label: String, _ n: Binding<String>, _ d: Binding<String>) -> some View {
        HStack { Text(label).frame(maxWidth: .infinity, alignment: .leading); TextField("BLAKE sats", text: n); Text("/"); TextField("BTC sats", text: d) }
    }
    private func authorization(_ review: Blakeswap_V1_AutomationReview) -> some View {
        let c = review.config
        return VStack(alignment: .leading, spacing: 12) {
            Text(review.enabled ? "Enable automatic trading" : "Save policy without enabling").font(.headline)
            Text("Policy \(c.id) · revision \(review.expectedRevision)").font(.caption.monospaced())
            Text("Sell \(c.sellAmount) \(c.sell.uppercased()) sats per whole offer; total gross limit \(c.volumeLimit) sats.")
            Text("BLAKE / BTC: fixed \(c.rate.numerator)/\(c.rate.denominator), minimum \(c.minRate.numerator)/\(c.minRate.denominator), maximum \(c.maxRate.numerator)/\(c.maxRate.denominator).")
            Text("Lifetime \(c.lifetime)s; check cadence \(c.cadence)s; maximum outstanding \(c.maxOpen).")
            Text("Funding fee \(c.fundingFee == 0 ? "fresh estimate" : String(c.fundingFee)), capped at \(c.maxFundingFee) sell-chain sats. Lifetime fee/rescue caps: \(c.btcFeeBudget) BTC sats; \(c.blakeFeeBudget) BLAKE sats.")
            Text("Rescue: \(c.towerBps) bps maximum; provider \(c.towerPubkey.isEmpty ? "off" : c.towerPubkey).")
            Text("Reference: \(c.reference); freshness \(c.referenceFreshness)s; maximum spread \(c.referenceSpreadBps) bps.")
            if !c.referenceMakers.isEmpty { Text(c.referenceMakers.joined(separator: "\n")).font(.caption.monospaced()).textSelection(.enabled) }
            Text("Existing volume: \(review.usage.committedVolume) committed + \(review.usage.reservedVolume) reserved.")
            Text("Existing BTC fee/rescue: \(review.usage.committedBtcFees) committed + \(review.usage.reservedBtcFees) reserved.")
            Text("Existing BLAKE fee/rescue: \(review.usage.committedBlakeFees) committed + \(review.usage.reservedBlakeFees) reserved.")
            Text(review.warning).font(.callout).foregroundStyle(.orange)
        }.textSelection(.enabled)
    }
}

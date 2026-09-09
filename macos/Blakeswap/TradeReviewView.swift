import SwiftUI

struct TakeOfferContext: Identifiable {
    let id = UUID()
    let order: Order
    let wallet: TradeContext
    var suggestedQuantity: Int64 = 0
    var eventID: String = ""
}

@MainActor
struct TradeComposer: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let context: TradeContext
    @State private var order: Order?
    let refreshParent: ((Order) async throws -> Blakeswap_V1_MarketOrder)?
    @State private var refreshingParent = false
    let management: ManageOfferContext?
    private let managementSeed: TradeManagementSeed?
    private let managementError: String?
    @StateObject private var review: TradeReviewModel
    @State private var fill = TradeFillDraft()
    @State private var expectedEventID: String
    @State private var sell = "btc"
    @State private var sellAmount = "1000000"
    @State private var buyAmount = "2000000"
    @State private var protection = false
    @State private var towerID = ""
    @State private var fundingFee = "2000"
    @State private var automaticFee = false
    @State private var feeReview: FeeReview?
    @State private var expires = Date().addingTimeInterval(86_400)

    init(context: TradeContext, root: String, order: Order? = nil, management: ManageOfferContext? = nil, suggestedQuantity: Int64 = 0, expectedEventID: String = "", refreshParent: ((Order) async throws -> Blakeswap_V1_MarketOrder)? = nil) {
        self.context = context; _order = State(initialValue: order); self.management = management; _expectedEventID = State(initialValue: expectedEventID); self.refreshParent = refreshParent
        var initial = TradeFillDraft()
        if let order { initial.seed(quantity: suggestedQuantity > 0 ? suggestedQuantity : (order.fillMode == "whole" ? order.sellAmount : 0)) }
        _fill = State(initialValue: initial)
        _review = StateObject(wrappedValue: TradeReviewModel(context: context, root: root))
        var seed: TradeManagementSeed?
        var seedError: String?
        if let management {
            do {
                guard management.wallet.matches(context), management.order.offer.network == context.network,
                      management.order.offer.maker == context.walletKey else {
                    throw RPCError.message("The source order does not match this wallet and network. Reopen it from the selected wallet's market history.")
                }
                seed = try TradeManagementSeed(action: management.action, source: management.order)
            } catch { seedError = error.localizedDescription }
            _sell = State(initialValue: seed?.sell ?? "")
            _sellAmount = State(initialValue: seed.map { String($0.sellAmount) } ?? "")
            _buyAmount = State(initialValue: seed.map { String($0.buyAmount) } ?? "")
            _fill = State(initialValue: seed?.fill ?? TradeFillDraft())
        }
        managementSeed = seed; managementError = seedError
    }
    private var matching: Bool { context.matches(model.tradeContext) && (management?.wallet.matches(context) ?? true) }

    private var paidChain: String { order?.buy ?? sell }
    private var paidAmount: String {
        guard let order else { return sellAmount }
        guard let q = try? TradeFillDraft.amount(fill.quantity, positive: true), let amount = try? roundedFillBuy(total: order.sellAmount, buy: order.buyAmount, quantity: q) else { return "" }
        return String(amount)
    }
    private var fillFeeScope: String { [fill.quantity, fill.mode, fill.minimum, fill.maximum, fill.btcFees, fill.blakeFees, fill.btcBounty, fill.blakeBounty, String(order?.revision ?? 0), order?.maker ?? "", order?.id ?? "", expectedEventID].joined(separator: "|") }
    private var replacement: ManageOfferContext? { management?.action == "replace" ? management : nil }
    private var feeKey: String { feeReviewKey(profile: context.profile, network: context.network, kind: "funding", chain: paidChain, amount: paidAmount, fee: fundingFee, automatic: automaticFee, generation: context.generation, sourceOfferID: replacement?.order.offer.id ?? "", sourceEventID: replacement?.order.eventID ?? "") }
    private var currentFee: FeeReview? { feeReview?.key == feeKey ? feeReview : nil }
    private var towers: [Blakeswap_V1_Tower] {
        let favorites = model.settings?.environments.first(where: { $0.network == context.network })?.favoriteWatchtowers ?? []
        return (model.status?.watchtowers ?? []).filter { favorites.contains($0.npub) && $0.expires > Int64(Date().timeIntervalSince1970) }
    }
    private var selectedTower: Blakeswap_V1_Tower? { towers.first { $0.pubkey == towerID } }
    private var validDraft: Bool {
        guard managementError == nil, !refreshingParent, currentFee != nil, !protection || selectedTower != nil else { return false }
        if let managementSeed, (try? managementSeed.validate(sell: sell, amount: sellAmount)) == nil { return false }
        var draft = Blakeswap_V1_TradeQuoteRequest(); draft.sellAmount = order?.sellAmount ?? (Int64(sellAmount) ?? 0)
        guard (try? fill.apply(to: &draft, order: order)) != nil else { return false }
        if order != nil { return true }
        guard expires > Date(), expires <= Date().addingTimeInterval(7 * 86_400) else { return false }
        guard let a = Int64(sellAmount), let b = Int64(buyAmount) else { return false }
        return (100_000...10_000_000_000).contains(a) && (100_000...10_000_000_000).contains(b)
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(review.pending != nil ? "Saved trade confirmation" : (management.map { $0.action == "replace" ? "Replace your offer" : "Recreate your offer" } ?? (order == nil ? "Create an offer" : "Take offer"))).font(.title2.bold())
            Text("Wallet \(model.settings?.wallets.first(where: { $0.id == context.profile })?.name ?? context.profile) · \(context.network.capitalized)").foregroundStyle(.secondary)
            if !matching {
                Text("The wallet or network changed. Close this window and reopen the review for the selected wallet.").foregroundStyle(.orange)
            } else if review.quote != nil || review.pending != nil {
                TradeEconomicsReview(review: review)
            } else {
                if let managementError {
                    Text(managementError).foregroundStyle(.orange)
                } else {
                    form.disabled(review.busy || review.journalBlocked || refreshingParent)
                }
                HStack {
                    Spacer()
                    Button("Review economics") { Task { await reviewDraft() } }
                        .buttonStyle(MintButton()).disabled(review.busy || review.journalBlocked || !validDraft)
                        .accessibilityIdentifier(order == nil ? "review-offer" : "review-take-offer")
                }
            }
            if let error = review.error { Text(error).font(.callout).foregroundStyle(.orange).textSelection(.enabled) }
            if review.busy { ProgressView("Checking the selected wallet…") }
            HStack {
                Button(review.pending == nil ? "Cancel" : "Close — confirmation is saved") { dismiss() }.disabled(review.busy)
                Spacer()
            }
        }.padding(28).frame(width: 620)
            .interactiveDismissDisabled(review.busy)
            .task {
                if order == nil, management == nil, !(model.status?.canReviewOffer(sell) ?? false), model.status?.canReviewOffer("blake") == true { sell = "blake" }
                towerID = towers.first?.pubkey ?? ""
            }
            .onChange(of: fill) { _, _ in feeReview = nil; review.invalidateDraft() }
            .onChange(of: protection) { _, _ in review.invalidateDraft() }
            .onChange(of: towerID) { _, _ in review.invalidateDraft() }
            .onChange(of: review.acceptedID) { _, id in
                guard let id, matching else { return }
                let kind = review.acceptedKind ?? review.quote?.kind ?? order.map { _ in "taker" } ?? "maker"
                model.notice = kind == "taker" ? "Swap request saved: \(id). Waiting for the maker to accept." : "Offer saved for publication: \(id)."
                if kind == "taker" { model.page = "Swaps" }
                Task { guard matching else { return }; await model.refresh() }
                dismiss()
            }
    }
    private var form: some View {
        VStack(alignment: .leading, spacing: 14) {
            if let order {
                Text("Available: \(order.available) \(symbol(order.sell)) sats · Bounds: \(order.minFill)–\(order.maxFill) · Signed revision \(order.revision)")
                TextField("Fill quantity (maker sell sats)", text: $fill.quantity).accessibilityIdentifier("fill-quantity")
                if let refreshParent {
                    Button("Refresh signed parent; keep quantity") {
                        Task {
                            refreshingParent = true; review.invalidateDraft(); feeReview = nil
                            defer { refreshingParent = false }
                            do {
                                let refreshed = try await refreshParent(order)
                                guard !Task.isCancelled, matching else { return }
                                try validateRefreshedParent(order, refreshed: refreshed)
                                self.order = refreshed.offer; expectedEventID = refreshed.eventID
                            } catch { if matching { review.error = error.localizedDescription } }
                        }
                    }
                }
                Text("Exact rounded payment preview: \(paidAmount.isEmpty ? "Unavailable" : paidAmount) \(symbol(order.buy)) sats. The daemon checks the remaining tail and current funds.").font(.caption)
            } else {
                if replacement != nil, let managementSeed {
                    Text("Replace available remainder: \(managementSeed.sellAmount) \(symbol(managementSeed.sell)) sats")
                        .accessibilityIdentifier("replacement-remainder")
                    Text("The sell asset and remaining quantity are fixed. Review a new receive price, fill bounds and private limits below.").font(.caption).foregroundStyle(.secondary)
                } else {
                    Picker("You sell", selection: $sell) {
                        Text("Bitcoin (BTC)").tag("btc")
                        Text("Bitcoin Blake2b (BLAKE)").tag("blake")
                    }
                    TextField("Sell principal (sats)", text: $sellAmount).accessibilityIdentifier("sell-amount")
                }
                TextField("Price-reference receive amount (sats)", text: $buyAmount).accessibilityIdentifier("buy-amount")
                Picker("Fill mode", selection: $fill.mode) { Text("Whole").tag("whole"); Text("Partial").tag("partial") }
                if fill.mode == "partial" {
                    TextField("Minimum fill (sell sats)", text: $fill.minimum)
                    TextField("Maximum fill (sell sats)", text: $fill.maximum)
                }
                Text("Hard monetary limits for this parent — enter fresh reviewed limits. These are authorizations, not up-front charges.").font(.caption)
                TextField("BTC fee budget (sats)", text: $fill.btcFees)
                TextField("BLAKE fee budget (sats)", text: $fill.blakeFees)
                TextField("BTC conditional bounty budget (sats)", text: $fill.btcBounty)
                TextField("BLAKE conditional bounty budget (sats)", text: $fill.blakeBounty)
                DatePicker("Offer expiry (up to 7 days)", selection: $expires, in: Date()...Date().addingTimeInterval(7 * 86_400), displayedComponents: [.date, .hourAndMinute]).accessibilityIdentifier("offer-expiry")
                if let management {
                    Text(management.action == "replace" ? "Confirming replaces the available remainder under fresh limits. Accepted children keep their original terms and continue settlement." : "This is a new order with a new ID. Funds, fees and optional protection are checked again.").font(.caption).foregroundStyle(.secondary)
                }
            }
            FeeQuoteControl(kind: "funding", chain: paidChain, amount: paidAmount, sourceOfferID: replacement?.order.offer.id ?? "", sourceEventID: replacement?.order.eventID ?? "", fee: $fundingFee, automatic: $automaticFee, review: $feeReview).id(fillFeeScope)
            Toggle("Protect my side with a watchtower", isOn: $protection)
            if protection {
                Picker("Favorite watchtower", selection: $towerID) {
                    Text("Select a watchtower").tag("")
                    ForEach(towers) { tower in Text(tower.label).tag(tower.pubkey) }
                }.accessibilityIdentifier("offer-watchtower")
                if towers.isEmpty { Text("Add an available favorite watchtower in Settings.").font(.caption).foregroundStyle(.secondary) }
            }
            Text("Review the net amounts, fee limits, timing and optional protection before authorizing the automatic swap sequence.").font(.caption).foregroundStyle(.secondary)
        }.textFieldStyle(.roundedBorder)
    }
    private func reviewDraft() async {
        guard matching, validDraft, let fee = currentFee else { return }
        var request = Blakeswap_V1_TradeQuoteRequest()
        request.kind = order == nil ? "maker" : "taker"
        request.maker = order?.maker ?? ""; request.id = order?.id ?? ""
        request.sell = order?.sell ?? sell
        request.sellAmount = order?.sellAmount ?? (Int64(sellAmount) ?? 0)
        request.buyAmount = order?.buyAmount ?? (Int64(buyAmount) ?? 0)
        if order == nil { request.expires = Int64(expires.timeIntervalSince1970) }
        request.fundingFee = fee.quote.fee; request.ownerFeeCap = 20_000
        if fee.automatic { request.rateSatKvb = fee.quote.estimate.rateSatKvb; request.feeTimestamp = fee.quote.estimate.timestamp }
        let tower = protection ? selectedTower : nil
        request.towerBps = tower?.bps ?? 0; request.towerPubkey = tower?.pubkey ?? ""
        do {
            try managementSeed?.applySource(to: &request)
            try fill.apply(to: &request, order: order)
        }
        catch { review.error = error.localizedDescription; return }
        await review.review(request, expectedEventID: expectedEventID, current: { model.tradeContext })
    }
}

private func policyWindow(_ value: UInt32, unit: String) -> String {
    if unit == "blocks" { return "\(value) blocks" }
    if value % 86_400 == 0 { return "\(value / 86_400) days" }
    return "\(value / 3_600) hours"
}
private func outcomeLabel(_ kind: String) -> String {
    switch kind {
    case "owner_claim": return "You claim"
    case "owner_refund": return "You refund"
    case "tower_claim": return "Tower rescues your incoming claim"
    case "tower_refund": return "Tower refunds your outgoing contract"
    default: return kind
    }
}

@MainActor
struct TradeEconomicsReview: View {
    @EnvironmentObject private var model: AppModel
    @ObservedObject var review: TradeReviewModel
    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { clock in
            VStack(alignment: .leading, spacing: 14) {
                ScrollView {
                    VStack(alignment: .leading, spacing: 12) {
                        if let q = review.quote {
                            if !q.orderAction.isEmpty {
                                Text(q.orderAction == "replace" ? "Replace and locally cancel your current order" : "Create a new order from your prior terms").font(.headline)
                                Text("Source order: \(q.sourceOfferID)").font(.caption.monospaced()).textSelection(.enabled)
                            }
                            Text("Review economics").font(.headline)
                            let economics = TradeEconomicsPresentation(quote: q)
                            Text("Pay: \(q.paidPrincipal) \(symbol(q.paidChain)) sats principal + \(economics.fundingReserve) sats funding reserve = \(q.paidTotal) \(symbol(q.paidChain)) sats.")
                            Text("\(economics.receivedLabel): \(q.receivedPrincipal) \(symbol(q.receivedChain)) sats.")
                            if economics.partialParent {
                                Text("Parent total \(q.totalSellAmount) · Available \(q.available) · Fill bounds \(q.minFill)–\(q.maxFill) sell sats")
                                Text("Independently rounded fills can receive a different total from this rate reference.").font(.caption)
                            }
                            ForEach(["btc", "blake"], id: \.self) { asset in
                                Text("\(symbol(asset)) limits: fees \(q.feeBudgets[asset].map(String.init) ?? "Not provided"), conditional bounties \(q.bountyBudgets[asset].map(String.init) ?? "Not provided") sats").font(.caption)
                            }
                            if economics.partialParent {
                                Text("Representative child — not aggregate parent outcomes").font(.headline)
                                Text("\(q.exampleFill.quantity) sell sats → \(q.exampleFill.buyAmount) buy sats · Funding fee \(q.exampleFill.fundingFee) · Owner fee cap \(q.exampleFill.ownerFeeCap)")
                            }
                            if q.kind == "taker" {
                                Text("Fill quantity: \(q.quantity) maker sell sats · Parent revision \(q.parentRevision)").font(.headline)
                                Text("Parent \(q.offerID) · Maker \(q.offerMaker)").font(.caption.monospaced()).textSelection(.enabled)
                            }
                            Text("Principal exchange rate ≈ \(q.rateDisplay) \(symbol(q.receivedChain)) per \(symbol(q.paidChain)) (exactly \(q.rateNumerator)/\(q.rateDenominator)).").font(.caption)
                            ForEach(economics.displayedOutcomes, id: \.kind) { outcome in
                                VStack(alignment: .leading, spacing: 3) {
                                    Text(outcomeLabel(outcome.kind)).font(.subheadline.bold())
                                    Text("Net receipt: \(outcome.netMin)–\(outcome.netMax) \(symbol(outcome.chain)) sats")
                                    Text("Mining fee: \(outcome.feeMin)–\(outcome.feeMax) sats · Conditional tower bounty: \(outcome.bounty) sats").font(.caption).foregroundStyle(.secondary)
                                }
                            }
                            Text("Refund outcomes return the outgoing principal after refund costs. The funding fee has already been spent. Fees and proceeds remain in their own assets.").font(.caption).foregroundStyle(.secondary)
                            Divider()
                            if q.provider.bps > 0 {
                                Text("Your provider: \(q.provider.name) · \(percentage(q.provider.bps)) only when its rescue confirms")
                                Text(q.provider.npub).font(.caption.monospaced()).textSelection(.enabled)
                                Text("Coverage: \(q.towerCoverage). No upfront tower charge.").font(.caption)
                            } else { Text("No watchtower selected. Keep the app open to respond before deadlines.").font(.caption) }
                            Text("Expected timing policy").font(.headline)
                            Text("\(q.timing.confirmations) confirmations required on each funding leg. Your refund window: \(policyWindow(q.timing.ownRefund, unit: q.timing.unit)); incoming contract: \(policyWindow(q.timing.incomingRefund, unit: q.timing.unit)).")
                            Text("The taker reveals first, within an expected \(policyWindow(q.timing.revealBefore, unit: q.timing.unit)) window. \(q.kind == "taker" ? "You are the taker and must make the first claim yourself." : "You claim after the taker reveals the secret.")")
                            if q.provider.bps > 0 {
                                if q.kind == "maker" { Text("Tower claim takeover policy: \(policyWindow(q.timing.towerTakeover, unit: q.timing.unit)).").font(.caption) }
                                Text("Tower refund grace: \(policyWindow(q.timing.refundGrace, unit: q.timing.unit)).").font(.caption)
                            }
                            Text("These windows are policy estimates from terms acceptance. Exact deadlines appear after negotiation; chain progress can change elapsed time.").font(.caption).foregroundStyle(.secondary)
                            Text("Funds check: \(q.funds.message)").font(.caption)
                            Text("Confirming authorizes automatic negotiation, funding and settlement within this reviewed policy. Once funded, the swap must settle or refund.").font(.callout)
                            if q.kind == "maker" { Text("The offer expires \(Date(timeIntervalSince1970: TimeInterval(q.offerExpires)).formatted()).").font(.caption) }
                            Text("Quote valid until \(Date(timeIntervalSince1970: TimeInterval(q.expires)).formatted(date: .omitted, time: .standard)).").font(.caption)
                        }
                        if let pending = review.pending {
                            Text("Saved confirmation: \(pending.requestID)").font(.caption.monospaced()).textSelection(.enabled)
                            Text("Check saved outcome reads an exact final receipt without a new prompt. Retry saved authorization asks for fresh authentication for this same saved request; it never creates a new confirmation identity.").font(.caption)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }.frame(maxHeight: 460)
                HStack {
                    if review.pending == nil { Button("Back") { review.back() }.disabled(review.busy) }
                    Spacer()
                    if review.pending != nil {
                        Button("Retry saved authorization") {
                            Task { await review.confirm(authorizeSaved: true, current: { model.tradeContext }) }
                        }.disabled(review.busy || !review.context.matches(model.tradeContext))
                    }
                    Button(review.pending == nil ? (review.quote?.orderAction == "replace" ? "Confirm replacement" : (review.quote?.kind == "maker" ? "Confirm and publish" : "Confirm swap request")) : "Check saved outcome") {
                        Task { await review.confirm(current: { model.tradeContext }) }
                    }.buttonStyle(MintButton())
                        .disabled(review.busy || !review.context.matches(model.tradeContext) || (review.pending == nil && (review.quote?.ready != true || (review.quote?.expires ?? 0) <= Int64(clock.date.timeIntervalSince1970))))
                        .accessibilityIdentifier(review.quote?.kind == "maker" ? "publish-offer" : "confirm-take-offer")
                }
            }
        }
    }
}

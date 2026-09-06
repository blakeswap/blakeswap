import SwiftUI

struct TakeOfferContext: Identifiable {
    let id = UUID()
    let order: Order
    let wallet: TradeContext
}

@MainActor
struct TradeComposer: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let context: TradeContext
    let order: Order?
    @StateObject private var review: TradeReviewModel
    @State private var sell = "btc"
    @State private var sellAmount = "1000000"
    @State private var buyAmount = "2000000"
    @State private var protection = false
    @State private var towerID = ""
    @State private var fundingFee = "2000"
    @State private var automaticFee = false
    @State private var feeReview: FeeReview?

    init(context: TradeContext, root: String, order: Order? = nil) {
        self.context = context; self.order = order
        _review = StateObject(wrappedValue: TradeReviewModel(context: context, root: root))
    }
    private var matching: Bool { context.matches(model.tradeContext) }
    private var paidChain: String { order?.buy ?? sell }
    private var paidAmount: String { order.map { String($0.buyAmount) } ?? sellAmount }
    private var feeKey: String { feeReviewKey(profile: context.profile, network: context.network, kind: "funding", chain: paidChain, amount: paidAmount, fee: fundingFee, automatic: automaticFee, generation: context.generation) }
    private var currentFee: FeeReview? { feeReview?.key == feeKey ? feeReview : nil }
    private var towers: [Blakeswap_V1_Tower] {
        let favorites = model.settings?.environments.first(where: { $0.network == context.network })?.favoriteWatchtowers ?? []
        return (model.status?.watchtowers ?? []).filter { favorites.contains($0.npub) && $0.expires > Int64(Date().timeIntervalSince1970) }
    }
    private var selectedTower: Blakeswap_V1_Tower? { towers.first { $0.pubkey == towerID } }
    private var validDraft: Bool {
        guard currentFee != nil, !protection || selectedTower != nil else { return false }
        if order != nil { return true }
        guard let a = Int64(sellAmount), let b = Int64(buyAmount) else { return false }
        return (100_000...10_000_000_000).contains(a) && (100_000...10_000_000_000).contains(b)
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(review.pending != nil ? "Saved trade confirmation" : (order == nil ? "Create an offer" : "Take offer")).font(.title2.bold())
            Text("Wallet \(model.settings?.wallets.first(where: { $0.id == context.profile })?.name ?? context.profile) · \(context.network.capitalized)").foregroundStyle(.secondary)
            if !matching {
                Text("The wallet or network changed. Close this window and reopen the review for the selected wallet.").foregroundStyle(.orange)
            } else if review.quote != nil || review.pending != nil {
                TradeEconomicsReview(review: review)
            } else {
                form.disabled(review.busy || review.journalBlocked)
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
                if order == nil, !(model.status?.canReviewOffer(sell) ?? false), model.status?.canReviewOffer("blake") == true { sell = "blake" }
                towerID = towers.first?.pubkey ?? ""
            }
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
                Text("Pay \(order.buyAmount) \(symbol(order.buy)) sats for a principal of \(order.sellAmount) \(symbol(order.sell)) sats.")
            } else {
                Picker("You sell", selection: $sell) {
                    Text("Bitcoin (BTC)").tag("btc")
                    Text("Bitcoin Blake2b (BLAKE)").tag("blake")
                }
                TextField("Sell principal (sats)", text: $sellAmount).accessibilityIdentifier("sell-amount")
                TextField("Receive principal (sats)", text: $buyAmount).accessibilityIdentifier("buy-amount")
            }
            FeeQuoteControl(kind: "funding", chain: paidChain, amount: paidAmount, fee: $fundingFee, automatic: $automaticFee, review: $feeReview)
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
        request.fundingFee = fee.quote.fee; request.ownerFeeCap = 20_000
        if fee.automatic { request.rateSatKvb = fee.quote.estimate.rateSatKvb; request.feeTimestamp = fee.quote.estimate.timestamp }
        let tower = protection ? selectedTower : nil
        request.towerBps = tower?.bps ?? 0; request.towerPubkey = tower?.pubkey ?? ""
        await review.review(request, current: { model.tradeContext })
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
                            Text("Review economics").font(.headline)
                            Text("Pay: \(q.paidPrincipal) \(symbol(q.paidChain)) sats principal + \(q.fees.fundingFee) sats funding fee = \(q.paidTotal) \(symbol(q.paidChain)) sats.")
                            Text("Receive principal: \(q.receivedPrincipal) \(symbol(q.receivedChain)) sats. Claim fees are deducted from this asset.")
                            Text("Principal exchange rate ≈ \(q.rateDisplay) \(symbol(q.receivedChain)) per \(symbol(q.paidChain)) (exactly \(q.rateNumerator)/\(q.rateDenominator)).").font(.caption)
                            ForEach(q.outcomes, id: \.kind) { outcome in
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
                            Text("Retrying checks this same authorized request, including after quote expiry or restart.").font(.caption)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }.frame(maxHeight: 460)
                HStack {
                    if review.pending == nil { Button("Back") { review.back() }.disabled(review.busy) }
                    Spacer()
                    Button(review.pending == nil ? (review.quote?.kind == "maker" ? "Confirm and publish" : "Confirm swap request") : "Retry saved confirmation") {
                        Task { await review.confirm(current: { model.tradeContext }) }
                    }.buttonStyle(MintButton())
                        .disabled(review.busy || !review.context.matches(model.tradeContext) || (review.pending == nil && (review.quote?.ready != true || (review.quote?.expires ?? 0) <= Int64(clock.date.timeIntervalSince1970))))
                        .accessibilityIdentifier(review.quote?.kind == "maker" ? "publish-offer" : "confirm-take-offer")
                }
            }
        }
    }
}

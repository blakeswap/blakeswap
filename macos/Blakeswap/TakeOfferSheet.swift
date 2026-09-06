import SwiftUI

struct TakeOfferSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let order: Order
    @State private var protection = false
    @State private var towerID = ""
    @State private var checkedFunds: FundsCheckKey?
    private var fundsReady: Bool {
        checkedFunds == FundsCheckKey(profile: model.profile, network: model.network, generation: model.generation,
                                     chain: order.buy, amount: order.buyAmount, fee: currentQuote?.quote.fee ?? 0)
    }
    @State private var fundingFee = "2000"
    @State private var automaticFee = false
    @State private var feeReview: FeeReview?
    private var feeKey: String { feeReviewKey(profile: model.profile, network: model.network, kind: "funding", chain: order.buy, amount: String(order.buyAmount), fee: fundingFee, automatic: automaticFee, generation: model.generation) }
    private var currentQuote: FeeReview? { feeReview?.key == feeKey ? feeReview : nil }
    private var towers: [Blakeswap_V1_Tower] {
        let favorites = model.settings?.environments.first(where: { $0.network == model.network })?.favoriteWatchtowers ?? []
        return (model.status?.watchtowers ?? []).filter { favorites.contains($0.npub) && $0.expires > Int64(Date().timeIntervalSince1970) }
    }
    private var selectedTower: Blakeswap_V1_Tower? { towers.first { $0.pubkey == towerID } }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Take offer").font(.title2.bold())
            Text("Sell \(units(order.buyAmount)) \(symbol(order.buy)) for \(units(order.sellAmount)) \(symbol(order.sell))")
            FeeQuoteControl(kind: "funding", chain: order.buy, amount: String(order.buyAmount), fee: $fundingFee, automatic: $automaticFee, review: $feeReview)
            Toggle("Protect my side with a watchtower", isOn: $protection)
            if protection {
                Picker("Favorite watchtower", selection: $towerID) {
                    Text("Select a watchtower").tag("")
                    ForEach(towers) { tower in Text(tower.label).tag(tower.pubkey) }
                }
                if towers.isEmpty { Text("Add an available watchtower to favorites in Settings.").font(.caption).foregroundStyle(.secondary) }
            }
            FundsPreflightView(chain: order.buy, amount: order.buyAmount, fee: currentQuote?.quote.fee ?? 0, ready: $checkedFunds)
            Text("Your protection choice is private to you and your watchtower. It covers your delayed refund; you must reveal the swap secret yourself. The fee is paid only if the tower's rescue confirms.").font(.caption).foregroundStyle(.secondary)
            if !protection { Text("Keep the app open to respond before the refund deadlines.").font(.caption).foregroundStyle(.secondary) }
            if let notice = model.notice, !notice.isEmpty { Text(notice).font(.caption).foregroundStyle(.secondary) }
            HStack {
                Spacer()
                Button("Cancel") { dismiss() }
                Button("Take offer") {
                    guard fundsReady, let quote = currentQuote else { return }
                    let tower = protection ? selectedTower : nil
                    let checked = checkedFunds
                    Task {
                        guard fundsReady, checked == checkedFunds else { return }
                        if await model.command("swap.take", quote.fundingParams.merging(["maker": order.maker, "id": order.id, "tower_bps": tower?.bps ?? 0, "tower_pubkey": tower?.pubkey ?? ""], uniquingKeysWith: { _, new in new })) { dismiss() }
                    }
                }.buttonStyle(MintButton()).disabled(model.busy || !fundsReady || currentQuote == nil || (protection && selectedTower == nil)).accessibilityIdentifier("confirm-take-offer")
            }
        }.padding(28).frame(width: 520)
    }
}

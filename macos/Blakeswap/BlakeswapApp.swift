import SwiftUI
import AppKit

@main
struct BlakeswapApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var model = AppModel()
    var body: some Scene {
        WindowGroup("Blakeswap") {
            ContentView().environmentObject(model)
                .frame(minWidth: 1060, minHeight: 730)
                .preferredColorScheme(.dark)
                .task {
                    model.start()
                    NSApp.activate(ignoringOtherApps: true)
                    while !Task.isCancelled {
                        await model.refresh()
                        // Avoid the cross-module generic Clock specialization crash:
                        // https://github.com/swiftlang/swift/issues/86204
                        try? await Task.sleep(nanoseconds: 1_500_000_000)
                    }
                }
        }
        .defaultSize(width: 1240, height: 840)
        .commands {
            CommandGroup(replacing: .help) {
                Button("Blakeswap Protocol & Setup") {
                    if let url = Bundle.main.resourceURL?.appendingPathComponent("docs/PROTOCOL.md") { NSWorkspace.shared.open(url) }
                }
            }
        }
    }
}

private let mint = Color(red: 0.39, green: 0.88, blue: 0.72)
private let panel = Color(red: 0.10, green: 0.12, blue: 0.15)
// Share the packaged Dock artwork instead of maintaining a second brand mark.
private let appIcon = Bundle.main.url(forResource: "AppIcon", withExtension: "icns")
    .flatMap { NSImage(contentsOf: $0) } ?? NSImage(size: .zero)

private struct MintButton: ButtonStyle {
    @Environment(\.isEnabled) private var isEnabled
    func makeBody(configuration: Configuration) -> some View {
        configuration.label.font(.callout.weight(.medium))
            .padding(.horizontal, 15).padding(.vertical, 10)
            .foregroundStyle(isEnabled ? .black : Color.white.opacity(0.35))
            .background(isEnabled ? mint.opacity(configuration.isPressed ? 0.75 : 1) : Color.white.opacity(0.05), in: RoundedRectangle(cornerRadius: 8))
    }
}

private struct OrderFilterTab: View {
    let filter: OrderFilter
    let count: Int
    let selected: Bool
    let action: () -> Void
    @State private var hovering = false
    @FocusState private var focused: Bool

    var body: some View {
        Button(action: action) {
            HStack(spacing: 9) {
                Text(filter.title).font(.system(size: 13, weight: .semibold))
                Text(count.formatted()).font(.system(size: 10, weight: .semibold, design: .rounded)).monospacedDigit()
                    .padding(.horizontal, 6).padding(.vertical, 3)
                    .background(selected ? mint.opacity(0.13) : Color.white.opacity(0.05), in: Capsule())
            }
            .foregroundStyle(selected ? mint : Color.white.opacity(hovering ? 0.88 : 0.55))
            .padding(.horizontal, 14).padding(.vertical, 10)
            .background(selected ? mint.opacity(0.08) : (hovering ? Color.white.opacity(0.035) : .clear), in: RoundedRectangle(cornerRadius: 9))
            .overlay { RoundedRectangle(cornerRadius: 9).strokeBorder(focused ? mint : (selected ? mint.opacity(0.22) : .clear), lineWidth: 1) }
            .contentShape(RoundedRectangle(cornerRadius: 9))
        }
        .buttonStyle(.plain).focused($focused).onHover { hovering = $0 }
        .accessibilityLabel(filter.rawValue).accessibilityValue("\(count) orders")
        .accessibilityAddTraits(selected ? .isSelected : [])
        .accessibilityIdentifier("order-filter-\(filter.key)")
    }
}

struct ContentView: View {
    @EnvironmentObject private var model: AppModel
    @State private var showOffer = false
    @State private var orderFilter: OrderFilter = .all
    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Divider()
            VStack(spacing: 0) {
                header
                Divider().opacity(0.5)
                ScrollView {
                    VStack(alignment: .leading, spacing: 24) {
                        if let error = model.connectionError {
                            VStack(alignment: .leading, spacing: 12) {
                                Label("Reconnecting to your wallet", systemImage: "network.slash").font(.headline)
                                Text(error).foregroundStyle(.secondary)
                            }.padding(24).frame(maxWidth: .infinity, alignment: .leading).background(panel, in: RoundedRectangle(cornerRadius: 16))
                        }
                        if model.page == "Settings", let settings = model.settings { SettingsView(settings: settings).id(settings.revision) }
                        else if let status = model.status {
                            if model.page == "Market" { market(status) }
                            else if model.page == "Swaps" { swaps(status) }
                            else { wallet(status) }
                        }
                        if let error = model.status?.lastError, !error.isEmpty { Text(error).font(.callout).foregroundStyle(.orange).textSelection(.enabled) }
                        if let notice = model.notice {
                            Label(notice, systemImage: "info.circle").font(.callout).foregroundStyle(mint).textSelection(.enabled)
                        }
                    }.padding(30)
                }
                footer
            }.background(Color(red: 0.065, green: 0.078, blue: 0.10))
        }
        .sheet(isPresented: $showOffer) { OfferSheet().environmentObject(model) }
        .sheet(isPresented: Binding(get: { model.recovery != nil }, set: { if !$0 { model.recovery = nil } })) {
            VStack(alignment: .leading, spacing: 20) {
                Text("Wallet recovery phrase").font(.title2.bold())
                Text("This restores keys for both chains. An encrypted state backup is also needed for pending swap secrets and signed rescue transactions.").foregroundStyle(.secondary)
                Text(model.recovery ?? "").font(.system(.body, design: .monospaced)).textSelection(.enabled).padding().background(panel, in: RoundedRectangle(cornerRadius: 12))
                Button("Done") { model.recovery = nil }.keyboardShortcut(.defaultAction)
            }.padding(32).frame(width: 520)
        }
    }

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 28) {
            HStack(spacing: 10) {
                Image(nsImage: appIcon).resizable().interpolation(.high).scaledToFit()
                    .frame(width: 34, height: 34).clipShape(RoundedRectangle(cornerRadius: 8))
                    .accessibilityHidden(true)
                Text("blakeswap").font(.system(size: 22, weight: .semibold, design: .rounded)).lineLimit(1).fixedSize()
            }.padding(.top, 14)
            VStack(alignment: .leading, spacing: 9) {
                Text("WALLET").font(.system(size: 10, weight: .semibold)).tracking(1.8).foregroundStyle(.secondary)
                Picker("Wallet", selection: Binding(get: { model.profile }, set: { model.selectProfile($0) })) {
                    ForEach(model.settings?.wallets ?? [], id: \.id) { wallet in Text(wallet.name).tag(wallet.id) }
                }.labelsHidden().pickerStyle(.menu).controlSize(.large).accessibilityIdentifier("wallet-picker").disabled(model.busy)
            }
            VStack(spacing: 6) {
                nav("Market", "chart.xyaxis.line")
                nav("Swaps", "arrow.triangle.2.circlepath")
                nav("Wallet", "wallet.bifold")
                nav("Settings", "gearshape")
            }
            Spacer()
            Text(model.network == "testnet" ? "TESTNET4" : model.network.uppercased()).font(.caption.bold()).foregroundStyle(mint)

        }.padding(22).frame(width: 220).background(Color(red: 0.085, green: 0.098, blue: 0.12))
    }
    private func nav(_ name: String, _ icon: String) -> some View {
        Button { model.page = name } label: {
            Label(name, systemImage: icon).font(.system(size: 14, weight: .medium)).frame(maxWidth: .infinity, alignment: .leading).padding(12)
                .foregroundStyle(model.page == name ? mint : .secondary)
                .background(model.page == name ? mint.opacity(0.09) : .clear, in: RoundedRectangle(cornerRadius: 9))
        }.buttonStyle(.plain)
    }
    private var header: some View {
        HStack {
            VStack(alignment: .leading, spacing: 5) {
                Text(model.page).font(.system(size: 27, weight: .semibold))

            }
            Spacer()
            if model.busy { ProgressView().controlSize(.small).padding(.trailing, 8) }

        }.padding(.horizontal, 30).padding(.vertical, 24)
    }
    private var footer: some View {
        HStack(spacing: 18) {
            Label("Queued messages: \(model.status?.pendingMessages ?? 0)", systemImage: "envelope")
            Text("BTC #\(model.status?.heights["btc"] ?? 0)")
            Text("BLAKE #\(model.status?.heights["blake"] ?? 0)")
            Spacer()
            if model.isRegtest { Button("Mine 2 blocks on both chains") { Task { await model.command("regtest.mine", ["blocks": 2]) } }
                .disabled(model.busy || model.status == nil).accessibilityIdentifier("mine-blocks") }
        }.font(.caption).foregroundStyle(.secondary).padding(.horizontal, 30).padding(.vertical, 14).background(panel.opacity(0.5))
    }
    private func balanceCard(_ chain: String, _ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Text(chain == "btc" ? "₿" : "B₂").font(.title3.bold()).foregroundStyle(chain == "btc" ? .orange : mint)
                Text(chain == "btc" ? "Bitcoin" : "Bitcoin Blake2b").font(.callout.weight(.medium))
                Spacer()
                Text(symbol(chain)).font(.caption2).foregroundStyle(.secondary)
            }
            Text(units(status.balances[chain] ?? 0)).font(.system(size: 28, weight: .medium, design: .rounded)).monospacedDigit()
            Text("Confirmed balance").font(.caption).foregroundStyle(.secondary)
        }.padding(22).frame(maxWidth: .infinity, alignment: .leading).background(panel, in: RoundedRectangle(cornerRadius: 14))
    }
    private func market(_ status: DaemonStatus) -> some View {
        let orders = orderFilter.orders(in: status)
        return VStack(alignment: .leading, spacing: 26) {
            HStack(spacing: 16) { balanceCard("btc", status); balanceCard("blake", status) }
            HStack {
                VStack(alignment: .leading, spacing: 5) {
                    Text("Orderbook").font(.title3.weight(.semibold))
                }
                Spacer()
                Button { showOffer = true } label: { Label("Create offer", systemImage: "plus") }.buttonStyle(MintButton()).disabled(model.busy || !["btc", "blake"].contains(where: status.canSell)).accessibilityIdentifier("create-offer")
            }
            HStack(spacing: 6) {
                ForEach(OrderFilter.allCases, id: \.self) { filter in
                    OrderFilterTab(filter: filter, count: filter.orders(in: status).count, selected: orderFilter == filter) { orderFilter = filter }
                }
                Spacer(minLength: 0)
            }
            .padding(.bottom, 10)
            .overlay(alignment: .bottom) { Rectangle().fill(Color.white.opacity(0.07)).frame(height: 1) }
            .accessibilityElement(children: .contain).accessibilityLabel("Show open orders").accessibilityIdentifier("order-filter")
            if !["btc", "blake"].contains(where: status.canSell) {
                Text("Deposit BTC or BLAKE and wait for confirmation to create an offer. The sell balance must cover the amount and funding fee.").font(.callout).foregroundStyle(.secondary)
            }
            if orders.isEmpty {
                ContentUnavailableView("No matching open orders", systemImage: "arrow.left.arrow.right")
                    .frame(maxWidth: .infinity).padding(28).background(panel.opacity(0.5), in: RoundedRectangle(cornerRadius: 14))
            } else {
                VStack(spacing: 0) {
                    HStack { Text("MAKER SELLS").frame(maxWidth: .infinity, alignment: .leading); Text("MAKER RECEIVES").frame(maxWidth: .infinity, alignment: .leading); Text("WATCHTOWER").frame(width: 160, alignment: .leading); Text("STATUS").frame(width: 124, alignment: .leading) }
                        .font(.system(size: 10, weight: .semibold)).tracking(1.2).foregroundStyle(.secondary).padding(18)
                    ForEach(orders, id: \.bookID) { order in
                        Divider().opacity(0.4)
                        HStack(spacing: 10) {
                            VStack(alignment: .leading, spacing: 6) { Text("\(units(order.sellAmount)) \(symbol(order.sell))").font(.system(.body, design: .monospaced)); Text(order.maker == status.pubkey ? "Your offer" : "Maker \(order.maker.prefix(10))…").font(.caption).foregroundStyle(.secondary) }.frame(maxWidth: .infinity, alignment: .leading)
                            Text("\(units(order.buyAmount)) \(symbol(order.buy))").font(.system(.body, design: .monospaced)).frame(maxWidth: .infinity, alignment: .leading)
                            VStack(alignment: .leading, spacing: 5) {
                                Text(order.towerBps > 0 ? "\(percentage(order.towerBps)) only if used" : "No protection")
                                if order.hasTower { Text(String(order.tower.npub.prefix(18)) + "…").font(.caption2.monospaced()).foregroundStyle(.secondary).help(order.tower.npub).textSelection(.enabled) }
                            }.frame(width: 160, alignment: .leading)
                            Group {
                                if order.status == "open" {
                                    if order.maker == status.pubkey { Button("Cancel") { Task { await model.command("offer.cancel", ["id": order.id]) } } }
                                    else { Button("Take offer") { Task { await model.command("swap.take", ["maker": order.maker, "id": order.id]) } }.tint(mint).accessibilityIdentifier("take-offer-\(order.id)") }
                                } else { Text(order.status.capitalized).foregroundStyle(order.status == "filled" ? mint : .secondary).font(.caption) }
                            }.frame(width: 124, alignment: .leading).disabled(model.busy)
                        }.padding(18)
                    }
                }.background(panel.opacity(0.6), in: RoundedRectangle(cornerRadius: 14))
            }
            Label("Quitting stops your daemon. Keep the app open during funded swaps unless a watchtower is armed.", systemImage: "clock.arrow.circlepath")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
    private func swaps(_ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: 18) {
            if status.swaps.isEmpty { ContentUnavailableView("No swaps yet", systemImage: "arrow.triangle.2.circlepath").frame(maxWidth: .infinity).padding(50) }
            ForEach(status.swaps) { swap in
                VStack(alignment: .leading, spacing: 18) {
                    HStack {
                        Image(systemName: swap.stage == "completed" ? "checkmark.circle.fill" : "arrow.triangle.2.circlepath").font(.title2).foregroundStyle(mint)
                        VStack(alignment: .leading, spacing: 5) { Text(swap.stage.capitalized).font(.title3.weight(.semibold)); Text("\(swap.role.capitalized) · \(swap.id.prefix(16))…").font(.caption.monospaced()).foregroundStyle(.secondary) }
                        Spacer()
                        Text(swap.towerReady ? "Tower armed" : swap.towerEnabled ? "Preparing protection" : "No tower armed").font(.caption).foregroundStyle(mint)
                    }
                    if swap.long.refundLocktime > 0 {
                        HStack(spacing: 20) { leg(swap.long, swap.longSpend, swap.longConfirmations); Image(systemName: "arrow.left.arrow.right").foregroundStyle(.secondary); leg(swap.short, swap.shortSpend, swap.shortConfirmations) }
                    } else { Text("Waiting for maker acceptance.").font(.callout).foregroundStyle(.secondary) }
                    Divider().opacity(0.5)
                    HStack(spacing: 24) {
                        metric("PREIMAGE", swap.secretRevealed ? "Released / observed" : "Private")
                        metric("TOWER TAKEOVER", swap.takeover > 0 ? locktimeLabel(swap.takeover) : "Negotiating")
                        metric("TOWER FEE PAID", swap.feeLabel)
                    }
                    if !swap.error.isEmpty { Text(swap.error).font(.caption).foregroundStyle(.orange).textSelection(.enabled) }
                }.padding(24).background(panel, in: RoundedRectangle(cornerRadius: 16))
            }
        }
    }
    private func leg(_ htlc: HTLC, _ spend: String?, _ confirmations: Int32) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("\(units(htlc.amount)) \(symbol(htlc.chain))").font(.system(.title3, design: .monospaced).weight(.medium))
            Text(spend == nil || spend == "" ? "Waiting for claim" : "Spend: \(confirmations) confirmations").font(.caption).foregroundStyle(.secondary)
            if !htlc.txid.isEmpty { Text("Lock  \(htlc.txid.prefix(18))…").font(.caption2.monospaced()).foregroundStyle(.secondary).help(htlc.txid).textSelection(.enabled) }
            if let spend, !spend.isEmpty { Text("Spend \(spend.prefix(18))…").font(.caption2.monospaced()).foregroundStyle(mint).help(spend).textSelection(.enabled) }
            Text("Refund eligible: \(locktimeLabel(htlc.refundLocktime))").font(.caption2).foregroundStyle(.secondary)
        }.frame(maxWidth: .infinity, alignment: .leading)
    }
    private func metric(_ title: String, _ value: String) -> some View { VStack(alignment: .leading, spacing: 7) { Text(title).font(.system(size: 9, weight: .semibold)).tracking(1).foregroundStyle(.secondary); Text(value).font(.caption) }.frame(maxWidth: .infinity, alignment: .leading) }
    private func wallet(_ status: DaemonStatus) -> some View {
        VStack(alignment: .leading, spacing: 24) {
            HStack(spacing: 16) { balanceCard("btc", status); balanceCard("blake", status) }
            ForEach(["btc", "blake"], id: \.self) { chain in
                VStack(alignment: .leading, spacing: 14) {
                    Text("Receive \(symbol(chain))").font(.headline)
                    Text(status.addresses[chain] ?? "").font(.system(.body, design: .monospaced)).textSelection(.enabled)
                    HStack {
                        Button("Copy address") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(status.addresses[chain] ?? "", forType: .string) }
                        if model.isRegtest { Button("Add 1 test coin") { Task { await model.command("regtest.faucet", ["chain": chain, "amount": 100_000_000]) } }.disabled(model.busy)
                        Text("Mine 2 blocks to confirm deposits.").font(.caption).foregroundStyle(.secondary) }
                    }
                }.padding(24).frame(maxWidth: .infinity, alignment: .leading).background(panel, in: RoundedRectangle(cornerRadius: 14))
            }
            VStack(alignment: .leading, spacing: 14) {
                Text("Recovery & protection").font(.headline)
                HStack { Button("Reveal recovery phrase") { Task { await model.command("wallet.recovery") } }; Button("Save encrypted state backup") { Task { await model.command("wallet.backup") } } }.disabled(model.busy)
                Text("Choose a favorite watchtower when creating an offer. Its rescue fee is paid only when used; mining fees are separate.").font(.caption).foregroundStyle(.secondary)
            }.padding(24).background(panel, in: RoundedRectangle(cornerRadius: 14))
        }
    }
}

struct OfferSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var sell = "btc"
    @State private var sellAmount = "1000000"
    @State private var buyAmount = "2000000"
    @State private var protection = false
    @State private var towerID = ""
    @State private var validation: String?
    private var favorites: [String] { model.settings?.environments.first(where: { $0.network == model.network })?.favoriteWatchtowers ?? [] }
    private var towers: [Blakeswap_V1_Tower] {
        (model.status?.watchtowers ?? []).filter { favorites.contains($0.npub) && $0.expires > Int64(Date().timeIntervalSince1970) }
    }
    private var selectedTower: Blakeswap_V1_Tower? { towers.first { $0.pubkey == towerID } }
    private var formError: String? {
        guard let status = model.status else { return "Waiting for your wallet balance." }
        if let error = status.offerValidation(sell: sell, sellAmount: sellAmount, buyAmount: buyAmount) { return error }
        if protection && selectedTower == nil { return "Select an available favorite watchtower. Add favorites in Settings." }
        return nil
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 20) {
            Text("Create an offer").font(.title.bold())
            Form {
                Picker("You sell", selection: $sell) {
                    Text("Bitcoin (BTC)").tag("btc").disabled(!(model.status?.canSell("btc") ?? false))
                    Text("Bitcoin Blake2b (BLAKE)").tag("blake").disabled(!(model.status?.canSell("blake") ?? false))
                }
                Text("Available: \(model.status?.balances[sell] ?? 0) \(symbol(sell)) sats · Funding fee: \(model.status?.offerFundingFee ?? 2_000) sats").font(.caption).foregroundStyle(.secondary)
                TextField("Sell amount (sats)", text: $sellAmount).accessibilityIdentifier("sell-amount")
                TextField("Receive amount (sats)", text: $buyAmount).accessibilityIdentifier("buy-amount")
                Toggle("Delayed watchtower protection", isOn: $protection)
                if protection {
                    Picker("Favorite watchtower", selection: $towerID) {
                        Text("Select a watchtower").tag("")
                        ForEach(towers) { tower in Text(tower.label).tag(tower.pubkey) }
                    }.accessibilityIdentifier("offer-watchtower")
                    if let tower = selectedTower { Text(tower.npub).font(.caption.monospaced()).textSelection(.enabled) }
                    if towers.isEmpty { Text("Add a public watchtower to favorites in Settings. Its announcement must be available on your relays.").font(.caption).foregroundStyle(.secondary) }
                }
            }.formStyle(.grouped)
            Text(protection ? "The tower earns \(percentage(selectedTower?.bps ?? 0)) only when its delayed rescue transaction confirms. Claim yourself first to avoid that fee." : "Keep the app open to respond before the refund deadlines. This offer will have no tower protection.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Text("Publishing authorizes your daemon to reserve this offer and fund the agreed swap after verifying the taker's confirmed contract. Expires in 24 hours.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let error = formError ?? validation { Text(error).foregroundStyle(.orange).font(.caption) }
            HStack {
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Spacer()
                Button("Publish offer") {
                    guard formError == nil, let a = Int64(sellAmount), let b = Int64(buyAmount) else { return }
                    let tower = protection ? selectedTower : nil
                    Task { if await model.command("offer.create", ["sell": sell, "sell_amount": a, "buy_amount": b, "tower_bps": tower?.bps ?? 0, "tower_pubkey": tower?.pubkey ?? ""]) { dismiss() } else { validation = model.notice } }
                }.buttonStyle(MintButton()).keyboardShortcut(.defaultAction).disabled(model.busy || formError != nil).accessibilityIdentifier("publish-offer")
            }
        }.padding(32).frame(width: 540)
        .task {
            if !(model.status?.canSell(sell) ?? false), model.status?.canSell("blake") == true { sell = "blake" }
            towerID = towers.first?.pubkey ?? ""
        }
    }
}

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

private struct MintButton: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label.font(.callout.weight(.medium))
            .padding(.horizontal, 15).padding(.vertical, 10)
            .foregroundStyle(.black)
            .background(mint.opacity(configuration.isPressed ? 0.75 : 1), in: RoundedRectangle(cornerRadius: 8))
    }
}

struct ContentView: View {
    @EnvironmentObject private var model: AppModel
    @State private var showOffer = false
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
                                Label("Local daemon disconnected", systemImage: "network.slash").font(.headline)
                                Text(error).foregroundStyle(.secondary)
                                Button("Restart daemon") { model.start() }.buttonStyle(.borderedProminent).tint(mint)
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
                Image(systemName: "arrow.left.arrow.right").font(.title3.bold()).foregroundStyle(.black).frame(width: 34, height: 34).background(mint, in: RoundedRectangle(cornerRadius: 10))
                Text("blakeswap").font(.system(size: 22, weight: .semibold, design: .rounded)).lineLimit(1).fixedSize()
            }.padding(.top, 14)
            VStack(alignment: .leading, spacing: 9) {
                if model.isRegtest { Text("WALLET").font(.system(size: 10, weight: .semibold)).tracking(1.8).foregroundStyle(.secondary) }
                if model.isRegtest { Picker("Wallet", selection: Binding(get: { model.profile }, set: { model.selectProfile($0) })) {
                    Text("Alice · maker").tag("alice")
                    Text("Bob · taker").tag("bob")
                }.labelsHidden().pickerStyle(.menu).controlSize(.large).accessibilityIdentifier("wallet-picker") }
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
            if let status = model.status {
                Label(status.paused ? "Trading paused" : "Daemon online", systemImage: status.paused ? "pause.circle" : "circle.fill")
                    .font(.caption).foregroundStyle(status.paused ? .orange : mint)
                Button(status.paused ? "Resume" : "Pause") { Task { await model.command("pause", ["paused": !status.paused]) } }.disabled(model.busy)
            }
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
        VStack(alignment: .leading, spacing: 26) {
            HStack(spacing: 16) { balanceCard("btc", status); balanceCard("blake", status) }
            HStack {
                VStack(alignment: .leading, spacing: 5) {
                    Text("Orderbook").font(.title3.weight(.semibold))
                }
                Spacer()
                Button { showOffer = true } label: { Label("Create offer", systemImage: "plus") }.buttonStyle(MintButton()).disabled(model.busy).accessibilityIdentifier("create-offer")
            }
            if status.orders.isEmpty {
                ContentUnavailableView("No offers yet", systemImage: "arrow.left.arrow.right")
                    .frame(maxWidth: .infinity).padding(28).background(panel.opacity(0.5), in: RoundedRectangle(cornerRadius: 14))
            } else {
                VStack(spacing: 0) {
                    HStack { Text("MAKER SELLS").frame(maxWidth: .infinity, alignment: .leading); Text("MAKER RECEIVES").frame(maxWidth: .infinity, alignment: .leading); Text("FALLBACK FEE").frame(width: 112, alignment: .leading); Text("STATUS").frame(width: 124, alignment: .leading) }
                        .font(.system(size: 10, weight: .semibold)).tracking(1.2).foregroundStyle(.secondary).padding(18)
                    ForEach(status.orders) { order in
                        Divider().opacity(0.4)
                        HStack(spacing: 10) {
                            VStack(alignment: .leading, spacing: 6) { Text("\(units(order.sellAmount)) \(symbol(order.sell))").font(.system(.body, design: .monospaced)); Text(order.maker == status.pubkey ? "Your offer" : "Maker \(order.maker.prefix(10))…").font(.caption).foregroundStyle(.secondary) }.frame(maxWidth: .infinity, alignment: .leading)
                            Text("\(units(order.buyAmount)) \(symbol(order.buy))").font(.system(.body, design: .monospaced)).frame(maxWidth: .infinity, alignment: .leading)
                            VStack(alignment: .leading, spacing: 5) { Text(percentage(order.towerBps)); Text("only if used").font(.caption2).foregroundStyle(.secondary) }.frame(width: 112, alignment: .leading)
                            Group {
                                if order.status == "open" {
                                    if order.maker == status.pubkey { Button("Cancel") { Task { await model.command("offer.cancel", ["id": order.id]) } } }
                                    else { Button("Take offer") { Task { await model.command("swap.take", ["maker": order.maker, "id": order.id]) } }.tint(mint).accessibilityIdentifier("take-offer-\(order.id)") }
                                } else { Text(order.status.capitalized).foregroundStyle(order.status == "filled" ? mint : .secondary).font(.caption) }
                            }.frame(width: 124, alignment: .leading).disabled(model.busy || status.paused)
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
                Text("Watchtower quote: \(percentage(status.tower.bps)) of the rescued local-chain output. No upfront fee. Mining fees are separate.").font(.caption).foregroundStyle(.secondary)
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
    @State private var validation: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 24) {
            Text("Create an offer").font(.title.bold())
            Form {
                Picker("You sell", selection: $sell) { Text("Bitcoin (BTC)").tag("btc"); Text("Bitcoin Blake2b (BLAKE)").tag("blake") }
                TextField("Sell amount (sats)", text: $sellAmount).accessibilityIdentifier("sell-amount")
                TextField("Receive amount (sats)", text: $buyAmount).accessibilityIdentifier("buy-amount")
                Toggle("Delayed watchtower protection", isOn: $protection).disabled(model.status?.tower.pubkey.isEmpty ?? true)
            }.formStyle(.grouped)
            Text(protection ? "The tower earns \(percentage(model.status?.tower.bps ?? 50)) only when its delayed rescue transaction confirms. Claim yourself first to avoid that fee." : "Your daemon must respond before the refund deadlines. This offer will have no tower protection.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Text("Publishing authorizes your daemon to reserve this offer and fund the agreed swap after verifying the taker's confirmed contract. Expires in 24 hours.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let validation { Text(validation).foregroundStyle(.orange).font(.caption) }
            HStack {
                Button("Cancel") { dismiss() }.keyboardShortcut(.cancelAction)
                Spacer()
                Button("Publish offer") {
                    guard let a = Int64(sellAmount), let b = Int64(buyAmount), a >= 100_000, b >= 100_000 else { validation = "Enter whole satoshi amounts of at least 100,000."; return }
                    Task { if await model.command("offer.create", ["sell": sell, "sell_amount": a, "buy_amount": b, "tower_bps": protection ? (model.status?.tower.bps ?? 0) : 0]) { dismiss() } else { validation = model.notice } }
                }.buttonStyle(MintButton()).keyboardShortcut(.defaultAction).disabled(model.busy).accessibilityIdentifier("publish-offer")
            }
        }.padding(32).frame(width: 510)
    }
}

import SwiftUI
import SwiftProtobuf

struct SendContext: Identifiable {
    let id = UUID()
    let profile: String
    let network: String
    let chain: String
}

extension Blakeswap_V1_WalletCoin {
    var outpointID: String { "\(txid):\(vout)" }
    func canSend(network: String) -> Bool { !reserved && confirmations >= (network == "regtest" ? 2 : 6) }
}

struct SendPlan {
    let request: Blakeswap_V1_SendCoinsRequest
    let total: Int64
    var change: Int64 { total - request.amount - request.fee }

    init(context: SendContext, destination: String, amount: String, fee: String,
         coins: [Blakeswap_V1_WalletCoin], selection: Set<String>) throws {
        guard let value = Int64(amount), (600...2_100_000_000_000_000).contains(value),
              let networkFee = Int64(fee), (1...1_000_000).contains(networkFee) else {
            throw RPCError.message("Enter an amount of at least 600 sats and a fee of 1–1,000,000 sats.")
        }
        let selected = coins.filter { selection.contains($0.outpointID) }
        guard !selected.isEmpty, selected.count <= 50, selected.count == selection.count,
              selected.allSatisfy({ $0.chain == context.chain && $0.canSend(network: context.network) }) else {
            throw RPCError.message("Select 1–50 unlocked, confirmed coins. Cancel an open order to release its coins.")
        }
        var sum: Int64 = 0
        for coin in selected {
            let added = sum.addingReportingOverflow(coin.amount)
            guard !added.overflow, coin.amount > 0, added.partialValue <= 2_100_000_000_000_000 else { throw RPCError.message("Invalid coin amount.") }
            sum = added.partialValue
        }
        guard sum >= value + networkFee else { throw RPCError.message("Selected coins do not cover the amount and fee.") }
        guard sum == value + networkFee || sum - value - networkFee >= 600 else {
            throw RPCError.message("Change would be below 600 sats. Adjust the amount or select another coin.")
        }
        let target = destination.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !target.isEmpty else { throw RPCError.message("Enter the recipient’s address.") }
        var request = Blakeswap_V1_SendCoinsRequest()
        request.id = UUID().uuidString
        request.chain = context.chain; request.expectedNetwork = context.network
        request.destination = target; request.amount = value; request.fee = networkFee
        request.inputs = selected.map { coin in
            var point = Blakeswap_V1_Outpoint(); point.txid = coin.txid; point.vout = coin.vout; return point
        }
        self.request = request; self.total = sum
    }
}

struct SendCoinsView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let context: SendContext
    @State private var destination = ""
    @State private var amount = ""
    @State private var fee = "1000"
    @State private var selection = Set<String>()
    @State private var reviewed: SendPlan?
    @State private var error: String?
    @State private var submitting = false

    private var matchingWallet: Bool { model.profile == context.profile && model.network == context.network }
    private var coins: [Blakeswap_V1_WalletCoin] { matchingWallet ? (model.status?.coins ?? []).filter { $0.chain == context.chain } : [] }
    private var selectedAmount: Int64 { coins.filter { selection.contains($0.outpointID) }.reduce(0) { $0 + $1.amount } }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Send \(symbol(context.chain))").font(.title2.bold())
            Text("\(context.network.capitalized) · Wallet \(model.settings?.wallets.first(where: { $0.id == context.profile })?.name ?? context.profile)")
                .foregroundStyle(.secondary)
            TextField("Recipient address", text: $destination).textFieldStyle(.roundedBorder)
                .accessibilityIdentifier("send-destination").disabled(reviewed != nil)
            HStack {
                VStack(alignment: .leading) { Text("Amount (sats)"); TextField("Amount", text: $amount).textFieldStyle(.roundedBorder) }
                VStack(alignment: .leading) { Text("Network fee (sats)"); TextField("Total fee", text: $fee).textFieldStyle(.roundedBorder) }
            }
            .disabled(reviewed != nil)
            Text("Coin control").font(.headline)
            Text("Locked coins belong to an open order, active trade, or pending send. Cancel an open order to unlock its coins.")
                .font(.caption).foregroundStyle(.secondary)
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    if coins.isEmpty { Text("No wallet coins available on this chain.").foregroundStyle(.secondary) }
                    ForEach(coins, id: \.outpointID) { coin in
                        Toggle(isOn: Binding(get: { selection.contains(coin.outpointID) }, set: { selected in
                            if selected { selection.insert(coin.outpointID) } else { selection.remove(coin.outpointID) }
                        })) {
                            VStack(alignment: .leading, spacing: 4) {
                                Text("\(coin.amount) sats · \(coin.confirmations) confirmations\(coin.reserved ? " · Locked" : "")")
                                Text(coin.outpointID).font(.caption.monospaced()).textSelection(.enabled)
                                Text(coin.address).font(.caption2.monospaced()).foregroundStyle(.secondary)
                            }
                        }.toggleStyle(.checkbox).disabled(reviewed != nil || !coin.canSend(network: context.network))
                    }
                }
            }.frame(minHeight: 150, maxHeight: 280)
            HStack {
                Text("Selected: \(selectedAmount) sats")
                Spacer()
                Button("Send selected minus fee") {
                    if let networkFee = Int64(fee), networkFee > 0, selectedAmount > networkFee { amount = String(selectedAmount - networkFee) }
                }
            }
            .disabled(reviewed != nil)
            if let reviewed {
                Divider()
                Text("Confirm send").font(.headline)
                Text("Send \(reviewed.request.amount) \(symbol(context.chain)) sats on \(context.network.capitalized) to:")
                Text(reviewed.request.destination).font(.body.monospaced()).textSelection(.enabled)
                Text("Network fee: \(reviewed.request.fee) sats · Change to your wallet: \(reviewed.change) sats")
                Text("This payment cannot be reversed after broadcast.").font(.caption).foregroundStyle(.secondary)
                HStack {
                    Button("Back") { self.reviewed = nil }
                    Spacer()
                    Button("Confirm and send") { Task { await submit(reviewed) } }.buttonStyle(.borderedProminent)
                }
            } else {
                HStack {
                    Button("Cancel") { dismiss() }
                    Spacer()
                    Button("Review send") {
                        do { reviewed = try SendPlan(context: context, destination: destination, amount: amount, fee: fee, coins: coins, selection: selection); error = nil }
                        catch { self.error = error.localizedDescription }
                    }.buttonStyle(.borderedProminent)
                }
            }
            if let error { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
            if !matchingWallet { Text("The selected wallet or network changed. Close this window and reopen Send.").foregroundStyle(.orange) }
        }.padding(28).frame(width: 640)
            .disabled(submitting || !matchingWallet)
            .interactiveDismissDisabled(submitting)
    }

    private func submit(_ plan: SendPlan) async {
        guard matchingWallet, !submitting else { return }
        submitting = true
        defer { submitting = false }
        do {
            let raw = try await DaemonRPC.call(root: model.root, profile: context.profile, method: "wallet.send", payload: plan.request.jsonUTF8Data())
            let sent = try Blakeswap_V1_WalletSend(serializedBytes: raw)
            model.notice = sent.submitted ? "Sent transaction: \(sent.txid)" : "Send saved for retry: \(sent.txid). \(sent.error)"
            await model.refresh(); dismiss()
        } catch { self.error = error.localizedDescription }
    }
}

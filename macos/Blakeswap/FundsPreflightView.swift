import SwiftUI
import SwiftProtobuf

// A fresh check is tied to the exact form, wallet and network. No result is
// retained across a context change, and the daemon repeats the signing gate.
struct FundsCheckKey: Hashable {
    let profile: String
    let network: String
    let generation: UInt64
    let chain: String
    let amount: Int64
    let fee: Int64
    var inputs: [String] = []
}

struct FundsPreflightView: View {
    @EnvironmentObject private var model: AppModel
    let chain: String
    let amount: Int64
    let fee: Int64
    var inputs: [Blakeswap_V1_Outpoint] = []
    @Binding var ready: FundsCheckKey?
    @State private var message = "Checking candidate funds…"
    @State private var retry = 0
    private var key: FundsCheckKey {
        FundsCheckKey(profile: model.profile, network: model.network, generation: model.generation,
                      chain: chain, amount: amount, fee: fee, inputs: inputs.map { "\($0.txid):\($0.vout)" })
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(message).font(.caption).foregroundStyle(.secondary)
            Button("Check funds again") { retry += 1 }.font(.caption)
        }
        .id(key)
        .task(id: "\(key)\(retry)") {
            ready = nil
            let checked = key
            guard amount >= 600, fee > 0 else { message = "Enter an amount and fee to check funds."; return }
            message = chain == "btc" ? "Checking BTC candidate inputs and replay ancestry…" : "Checking unlocked confirmed candidate inputs…"
            do {
                var request = Blakeswap_V1_FundsPreflightRequest()
                request.chain = chain; request.amount = amount; request.fee = fee
                request.inputs = inputs; request.expectedNetwork = checked.network
                let raw = try await DaemonRPC.call(root: model.root, profile: checked.profile, method: "wallet.preflight", payload: request.jsonUTF8Data())
                let result = try Blakeswap_V1_FundsPreflight(serializedBytes: raw)
                guard !Task.isCancelled, checked == key, result.wallet == checked.profile, result.network == checked.network else { return }
                message = result.message
                ready = result.sufficient && ["proven", "not_applicable"].contains(result.state) ? checked : nil
            } catch {
                guard !Task.isCancelled, checked == key else { return }
                message = "Funds check unavailable: \(error.localizedDescription)"
            }
        }
    }
}

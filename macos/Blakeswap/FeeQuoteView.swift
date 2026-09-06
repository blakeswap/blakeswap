import SwiftUI
import SwiftProtobuf

struct FeeReview {
    let key: String
    let quote: Blakeswap_V1_FeeQuote
    let automatic: Bool
    var fundingParams: [String: Any] {
        ["funding_fee": quote.fee, "rate_sat_kvb": automatic ? quote.estimate.rateSatKvb : 0,
         "fee_timestamp": automatic ? quote.estimate.timestamp : 0, "owner_fee_cap": 20_000]
    }
}

func feeReviewKey(profile: String, network: String, kind: String, chain: String, amount: String,
                  destination: String = "", fee: String, automatic: Bool, generation: UInt64 = 0, inputs: [Blakeswap_V1_Outpoint] = []) -> String {
    [profile, network, String(generation), kind, chain, amount, destination, fee, String(automatic),
     inputs.map { "\($0.txid):\($0.vout)" }.sorted().joined(separator: ",")].joined(separator: "|")
}

struct FeeQuoteControl: View {
    @EnvironmentObject private var model: AppModel
    let kind: String
    let chain: String
    let amount: String
    var destination = ""
    var inputs: [Blakeswap_V1_Outpoint] = []
    @Binding var fee: String
    @Binding var automatic: Bool
    @Binding var review: FeeReview?
    @State private var error: String?
    @State private var refreshID = 0
    private var key: String { feeReviewKey(profile: model.profile, network: model.network, kind: kind, chain: chain, amount: amount, destination: destination, fee: fee, automatic: automatic, generation: model.generation, inputs: inputs) }
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Toggle("Use this chain’s fee estimate (6 blocks)", isOn: $automatic)
            if !automatic { TextField("Manual total fee (native sats)", text: $fee).textFieldStyle(.roundedBorder) }
            HStack {
                Text("\(symbol(chain)) fees").font(.caption.bold())
                Spacer()
                Button("Refresh fee") { refreshID += 1 }
            }
            if let review, review.key == key {
                let q = review.quote
                Text("Recipient / contract: \(q.amount) sats · Mining fee: \(q.fee) sats · Change: \(q.change) sats")
                Text("Size ≤ \(q.vsize) vB · Selected rate ≥ \(q.fee * 1000 / max(q.vsize, 1)) native sat/kvB")
                if q.estimate.state == "available" { Text("Estimate: \(q.estimate.rateSatKvb) native sat/kvB · \(q.estimate.target) blocks · \(q.estimate.source)") }
                if q.estimate.state != "available" { Text("Estimate \(q.estimate.state): \(q.estimate.error). Manual fee selected.").foregroundStyle(.orange) }
                if kind == "funding" { Text("Owner settlement: 2,000–20,000 native sats per claim/refund. Tower rescue uses the same signed fee cap, plus its agreed bounty. Funding cannot be accelerated by replacement.") }
            } else if let error { Text(error).foregroundStyle(.orange) }
            else { Text("Enter valid amounts and select coins to review the fee.").foregroundStyle(.secondary) }
        }.font(.caption)
            .task(id: key + "|\(refreshID)") { await quote() }
    }
    private func quote() async {
        let boundKey = key, profile = model.profile, network = model.network
        review = nil; error = nil
        guard let amountValue = Int64(amount), amountValue >= 600,
              automatic || (Int64(fee) ?? 0) > 0 else { return }
        var request = Blakeswap_V1_FeeQuoteRequest()
        request.kind = kind; request.chain = chain; request.amount = amountValue
        request.destination = destination; request.inputs = inputs
        request.fee = automatic ? 0 : (Int64(fee) ?? 0); request.target = 6; request.expectedNetwork = network
        do {
            let raw = try await DaemonRPC.call(root: model.root, profile: profile, method: "fee.quote", payload: request.jsonUTF8Data())
            let quote = try Blakeswap_V1_FeeQuote(serializedBytes: raw)
            guard !Task.isCancelled, model.profile == profile, model.network == network, key == boundKey else { return }
            guard quote.error.isEmpty, quote.fee > 0 else { error = quote.error.isEmpty ? "Fee unavailable. Select a manual total fee." : quote.error; return }
            review = FeeReview(key: boundKey, quote: quote, automatic: automatic)
        } catch {
            guard !Task.isCancelled, model.profile == profile, model.network == network, key == boundKey else { return }
            self.error = error.localizedDescription
        }
    }
}

struct AccelerateSendControl: View {
    @EnvironmentObject private var model: AppModel
    let send: Blakeswap_V1_WalletSend
    @State private var fee = ""
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("\(send.state.capitalized) · Authorized maximum: \(send.maxFee) sats · \(send.variants.count) signed variants")
            if send.confirmations == 0 && send.maxFee > send.fee && send.change >= 600 {
                HStack {
                    TextField("New total fee (sats)", text: $fee).frame(width: 180)
                    Button("Increase fee") {
                        guard let value = Int64(fee) else { return }
                        Task { await model.command("transaction.bump", ["kind": "send", "id": send.id, "fee": value, "expected_txid": send.txid]) }
                    }.disabled(model.busy || (Int64(fee) ?? 0) <= send.fee || (Int64(fee) ?? 0) > send.maxFee)
                }
                Text("The extra fee comes from change. The recipient amount stays fixed.")
            }
            ForEach(send.variants, id: \.txid) { variant in
                Text("\(variant.txid) · \(variant.fee) sats · \(variant.confirmations) confirmations").textSelection(.enabled)
            }
        }.font(.caption)
    }
}

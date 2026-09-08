import SwiftUI

struct CapacityHealthView: View {
    let health: Blakeswap_V2_CapacityHealth
    private func bytes(_ value: UInt64) -> String { ByteCountFormatter.string(fromByteCount: Int64(clamping: value), countStyle: .binary) }
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Wallet capacity · \(health.state.capitalized)").font(.headline)
            Text("\(health.active) active obligations · \(health.archived) encrypted archive records").font(.caption)
            Text("\(bytes(health.activeBytes)) active data · \(bytes(health.budgetBytes)) working budget").font(.caption).foregroundStyle(.secondary)
            Text("\(bytes(health.retainedBytes)) encrypted history · \(bytes(health.reservedBytes)) estimated continuation space").font(.caption).foregroundStyle(.secondary)
            Text(health.diskKnown ? "\(bytes(health.availableDiskBytes)) disk space currently available" : "Disk availability could not be verified.").font(.caption).foregroundStyle(.secondary)
            Text(health.message).font(.caption).foregroundStyle(health.admissionAvailable ? Color.secondary : Color.orange)
            if health.publicLimited { Text("Public order discovery reached its independent capacity. Known order updates and established wallet messages remain enabled; the displayed market is incomplete.").font(.caption).foregroundStyle(.orange) }
            DisclosureGroup("Relay synchronization") {
                ForEach(Array(health.relays.enumerated()), id: \.offset) { _, relay in
                    VStack(alignment: .leading, spacing: 4) {
                        Text("\(relay.filter.capitalized) · \(relay.live ? "Live connected" : "Reconnecting") · \(relay.cursor.sweeps) history sweeps").font(.caption)
                        Text(relay.relay).font(.caption2).textSelection(.enabled)
                        Text(relay.cursor.incomplete.isEmpty ? "Historical coverage is not yet verified." : relay.cursor.incomplete).font(.caption2).foregroundStyle(.secondary)
                        if !relay.error.isEmpty { Text(relay.error).font(.caption2).foregroundStyle(.orange) }
                    }.padding(.vertical, 5)
                }
            }
            Text("Activity history includes archived records. Archives retain signed recovery evidence; a deep reorg can require their reactivation.").font(.caption).foregroundStyle(.secondary)
        }.accessibilityIdentifier("wallet-capacity")
    }
}

struct RetainedRecordPresentation: Identifiable {
    let detail: Blakeswap_V2_RecordDetail
    let context: TradeContext
    var id: String { detail.kind + "/" + detail.id }
}
struct RetainedRecordView: View {
    @EnvironmentObject private var app: AppModel
    let record: RetainedRecordPresentation
    @Environment(\.dismiss) private var dismiss
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("\(record.detail.kind.capitalized) details").font(.title2.bold())
            Text("\(record.context.profile) · \(record.context.network)").foregroundStyle(.secondary)
            if !record.detail.message.isEmpty { Text(record.detail.message).font(.callout).foregroundStyle(record.detail.monitoringRequired ? Color.orange : Color.secondary) }
            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    Text(record.detail.id).font(.caption.monospaced())
                    if record.detail.hasSwap {
                        let swap = record.detail.swap
                        Text("\(swap.role.capitalized) · \(swap.stage)").font(.headline)
                        Text("Long leg: \(swap.long.amount) \(symbol(swap.long.chain)) sats · \(swap.longConfirmations) confirmations")
                        Text("Short leg: \(swap.short.amount) \(symbol(swap.short.chain)) sats · \(swap.shortConfirmations) confirmations")
                        Text("Long funding: \(swap.long.txid)\nLong spend: \(swap.longSpend)\nShort funding: \(swap.short.txid)\nShort spend: \(swap.shortSpend)").font(.caption.monospaced())
                        if !swap.parentID.isEmpty, !swap.parentMaker.isEmpty {
                            Text("Fill \(swap.quantity) sell sats · Parent revision \(swap.parentRevision)")
                            Text(swap.allocationKnown ? "\(swap.allocation.capitalized): \(swap.allocatedQuantity) sell sats currently allocated" : "Current maker allocation unknown")
                            Button("Show parent fills") {
                                guard record.context.matches(app.tradeContext) else { return }
                                app.activityDestination = .order(swap.parentID, maker: swap.parentMaker); app.page = "Market"; dismiss()
                            }.disabled(!record.context.matches(app.tradeContext))
                        }
                        Text("Owner fee cap: \(swap.ownerFeeCap) sats · Tower paid: \(swap.feeLabel)")
                        Text(swap.secretRevealed ? "Preimage release/observation is retained." : "No preimage release recorded.")
                        if !swap.error.isEmpty { Text(swap.error).foregroundStyle(.orange) }
                    }
                    if record.detail.hasSend {
                        let send = record.detail.send
                        Text("\(send.amount) \(symbol(send.chain)) sats · \(send.state)").font(.headline)
                        Text("Destination: \(send.destination)\nTransaction: \(send.txid)").font(.caption.monospaced())
                        Text("Fee: \(send.fee) sats · \(send.confirmations) confirmations")
                        ForEach(send.variants, id: \.txid) { variant in Text("\(variant.txid) · \(variant.fee) sats").font(.caption.monospaced()) }
                        if !send.error.isEmpty { Text(send.error).foregroundStyle(.orange) }
                    }
                    if record.detail.hasTowerJob {
                        let job = record.detail.towerJob
                        Text("\(job.kind) · \(symbol(job.chain)) · \(job.confirmations) confirmations")
                        Text("Swap: \(job.swapID)\nBroadcast: \(job.broadcast)").font(.caption.monospaced())
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
            }.frame(maxHeight: 450)
            Button("Done") { dismiss() }.keyboardShortcut(.cancelAction)
        }.padding(28).frame(width: 650)
    }
}

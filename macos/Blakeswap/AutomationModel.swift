import Foundation
import SwiftProtobuf

extension Blakeswap_V2_AutomationView: Identifiable { var id: String { config.id } }

struct AutomationDraft: Identifiable {
    var id: String
    var revision: UInt64 = 0
    var sell = "blake"
    var size = "1000000"
    var volume = "1000000"
    var rateN = "1", rateD = "1", minN = "1", minD = "1", maxN = "1", maxD = "1"
    var lifetime = "3600", cadence = "300", maxOpen = "1"
    var funding = "0", fundingCap = "10000", btcBudget = "30000", blakeBudget = "30000"
    var towerBPS = "0", towerKey = ""
    var reference = "fixed", makers = "", freshness = "120", spread = "100"
    var enabled = false
    var restored = false
    var acknowledgeRestored = false
    init(_ policy: Blakeswap_V2_AutomationView? = nil) {
        // New policy IDs are independent from every old policy and offer ID.
        id = UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased() + UUID().uuidString.replacingOccurrences(of: "-", with: "").lowercased()
        guard let policy else { return }
        let c = policy.config
        id = c.id; revision = policy.revision; sell = c.sell; size = String(c.sellAmount); volume = String(c.volumeLimit)
        rateN = String(c.rate.numerator); rateD = String(c.rate.denominator)
        minN = String(c.minRate.numerator); minD = String(c.minRate.denominator)
        maxN = String(c.maxRate.numerator); maxD = String(c.maxRate.denominator)
        lifetime = String(c.lifetime); cadence = String(c.cadence); maxOpen = String(c.maxOpen)
        funding = String(c.fundingFee); fundingCap = String(c.maxFundingFee)
        btcBudget = String(c.btcFeeBudget); blakeBudget = String(c.blakeFeeBudget)
        towerBPS = String(c.towerBps); towerKey = c.towerPubkey
        reference = c.reference; makers = c.referenceMakers.joined(separator: "\n")
        freshness = String(c.referenceFreshness); spread = String(c.referenceSpreadBps)
        enabled = policy.enabled; restored = policy.restoreHold
    }
    func edit(context: TradeContext) throws -> Blakeswap_V2_AutomationEdit {
        func number(_ text: String, _ label: String) throws -> Int64 {
            guard let n = Int64(text), n >= 0 else { throw RPCError.message("\(label) must be an exact nonnegative whole number.") }
            return n
        }
        func rate(_ n: String, _ d: String) throws -> Blakeswap_V2_AutomationRate {
            var r = Blakeswap_V2_AutomationRate(); r.numerator = try number(n, "Rate numerator"); r.denominator = try number(d, "Rate denominator")
            guard r.numerator > 0, r.denominator > 0 else { throw RPCError.message("Rate fractions must be positive.") }
            return r
        }
        var c = Blakeswap_V2_AutomationConfig()
        c.id = id; c.wallet = context.profile; c.network = context.network; c.sell = sell
        c.sellAmount = try number(size, "Offer size"); c.volumeLimit = try number(volume, "Total sell volume")
        c.rate = try rate(rateN, rateD); c.minRate = try rate(minN, minD); c.maxRate = try rate(maxN, maxD)
        c.lifetime = try number(lifetime, "Lifetime"); c.cadence = try number(cadence, "Cadence")
        let count = try number(maxOpen, "Maximum open offers")
        guard (1...8).contains(count) else { throw RPCError.message("Maximum open offers must be 1–8.") }
        c.maxOpen = UInt32(count); c.fundingFee = try number(funding, "Funding fee"); c.maxFundingFee = try number(fundingCap, "Funding cap")
        c.btcFeeBudget = try number(btcBudget, "BTC fee budget"); c.blakeFeeBudget = try number(blakeBudget, "BLAKE fee budget")
        c.towerBps = try number(towerBPS, "Rescue basis points"); c.towerPubkey = towerKey.trimmingCharacters(in: .whitespacesAndNewlines)
        c.reference = reference
        if reference == "orderbook" {
            c.referenceMakers = makers.split(whereSeparator: { $0.isWhitespace || $0 == "," }).map(String.init)
            c.referenceFreshness = try number(freshness, "Reference freshness"); c.referenceSpreadBps = try number(spread, "Maximum reference spread")
        }
        var p = Blakeswap_V2_AutomationEdit(); p.config = c; p.expectedRevision = revision; p.enabled = enabled
        p.acknowledgeRestoredBudget = acknowledgeRestored
        return p
    }
}

@MainActor
final class AutomationModel: ObservableObject {
    @Published private(set) var policies: [Blakeswap_V2_AutomationView] = []
    @Published private(set) var busy = false
    @Published private(set) var loaded = false
    @Published private(set) var error: String?
    @Published private(set) var review: Blakeswap_V2_AutomationReview?
    private var reviewedEdit: Blakeswap_V2_AutomationEdit?
    let context: TradeContext
    private let call: (String, Data) async throws -> Data
    init(context: TradeContext, root: String, call: ((String, Data) async throws -> Data)? = nil) {
        self.context = context
        self.call = call ?? { method, data in try await DaemonRPC.call(root: root, profile: context.profile, method: method, payload: data) }
    }
    func load(current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil; defer { busy = false }
        do {
            var q = Blakeswap_V2_AutomationQuery(); q.expectedWallet = context.profile; q.expectedNetwork = context.network
            let data = try await call("automation.list", q.jsonUTF8Data())
            let page = try Blakeswap_V2_AutomationList(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard page.wallet == context.profile, page.network == context.network,
                  page.policies.allSatisfy({ $0.config.wallet == context.profile && $0.config.network == context.network }) else { throw RPCError.message("Automation wallet or network changed; reload.") }
            policies = page.policies; loaded = true
        } catch { if context.matches(current()) { self.error = error.localizedDescription } }
    }
    func review(_ draft: AutomationDraft, current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil; review = nil; reviewedEdit = nil; defer { busy = false }
        do {
            var edit = try draft.edit(context: context)
            let data = try await call("automation.review", edit.jsonUTF8Data())
            let result = try Blakeswap_V2_AutomationReview(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard result.config == edit.config, result.expectedRevision == edit.expectedRevision,
                  result.enabled == edit.enabled, !result.reviewDigest.isEmpty else { throw RPCError.message("Policy review changed. Review the current authorization again.") }
            edit.reviewDigest = result.reviewDigest; reviewedEdit = edit; review = result
        } catch { if context.matches(current()) { self.error = error.localizedDescription } }
    }
    func revise() { review = nil; reviewedEdit = nil; error = nil }
    func save(current: () -> TradeContext) async -> Bool {
        guard !busy, context.matches(current()), let edit = reviewedEdit else { return false }
        busy = true; error = nil; defer { busy = false }
        do {
            let data = try await call("automation.save", edit.jsonUTF8Data())
            let result = try Blakeswap_V2_AutomationView(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()), result.config == edit.config,
                  result.revision == edit.expectedRevision + 1, result.enabled == edit.enabled else { throw RPCError.message("Save response changed. Refresh policy status before retrying.") }
            review = nil; reviewedEdit = nil; return true
        } catch { if context.matches(current()) { self.error = "\(error.localizedDescription) Refresh policies to resolve an uncertain save; the same policy ID cannot create a duplicate." }; return false }
    }
    func disable(_ policy: Blakeswap_V2_AutomationView, cancelOpen: Bool, current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil
        var q = Blakeswap_V2_DisableAutomationRequest(); q.id = policy.id; q.expectedWallet = context.profile; q.expectedNetwork = context.network
        q.expectedRevision = policy.revision; q.cancelOpen = cancelOpen
        do {
            _ = try await call("automation.disable", q.jsonUTF8Data()); busy = false
            await load(current: current)
        } catch { busy = false; if context.matches(current()) { self.error = error.localizedDescription } }
    }
}

import Foundation
import SwiftProtobuf

extension Blakeswap_V2_StrategyView: Identifiable { var id: String { config.id } }

struct StrategySideDraft {
    var target = "5000000", reserve = "500000", exposure = "2000000"
    var minimum = "100000", maximum = "1000000", volume = "5000000"
    var funding = "0", fundingCap = "10000"
    init(_ side: Blakeswap_V2_StrategySide? = nil) {
        guard let side else { return }
        target = String(side.target); reserve = String(side.minimumReserve); exposure = String(side.maxExposure)
        minimum = String(side.minOffer); maximum = String(side.maxOffer); volume = String(side.volumeLimit)
        funding = String(side.fundingFee); fundingCap = String(side.maxFundingFee)
    }
    func value() throws -> Blakeswap_V2_StrategySide {
        var s = Blakeswap_V2_StrategySide()
        s.target = try strategyInteger(target); s.minimumReserve = try strategyInteger(reserve); s.maxExposure = try strategyInteger(exposure)
        s.minOffer = try strategyInteger(minimum); s.maxOffer = try strategyInteger(maximum); s.volumeLimit = try strategyInteger(volume)
        s.fundingFee = try strategyInteger(funding); s.maxFundingFee = try strategyInteger(fundingCap)
        return s
    }
}
func strategyInteger(_ text: String) throws -> Int64 {
    guard let n = Int64(text), n >= 0 else { throw RPCError.message("Use exact nonnegative whole satoshis or whole-number limits.") }
    return n
}
struct StrategyDraft: Identifiable {
    var common = AutomationDraft()
    var btc = StrategySideDraft(), blake = StrategySideDraft()
    var spread = "100", minSpread = "50", maxSpread = "200", skew = "50"
    var concurrent = "2", failures = "3", replacements = "2", failureRate = "8000"
    var id: String { common.id }
    init(_ strategy: Blakeswap_V2_StrategyView? = nil) {
        common.minN = "1"; common.minD = "2"; common.maxN = "2"; common.maxD = "1"
        common.btcBudget = "300000"; common.blakeBudget = "300000"
        guard let strategy else { return }
        let c = strategy.config
        common.id = c.id; common.revision = strategy.revision; common.enabled = strategy.enabled; common.restored = strategy.restoreHold
        common.rateN = String(c.rate.numerator); common.rateD = String(c.rate.denominator)
        common.minN = String(c.minRate.numerator); common.minD = String(c.minRate.denominator)
        common.maxN = String(c.maxRate.numerator); common.maxD = String(c.maxRate.denominator)
        common.cadence = String(c.cadence); common.lifetime = String(c.lifetime)
        common.btcBudget = String(c.btcFeeBudget); common.blakeBudget = String(c.blakeFeeBudget)
        common.towerBPS = String(c.towerBps); common.towerKey = c.towerPubkey
        common.reference = c.reference; common.makers = c.referenceMakers.joined(separator: "\n")
        common.freshness = String(c.referenceFreshness); common.spread = String(c.referenceSpreadBps)
        btc = StrategySideDraft(c.btc); blake = StrategySideDraft(c.blake)
        spread = String(c.spreadBps); minSpread = String(c.minSpreadBps); maxSpread = String(c.maxSpreadBps); skew = String(c.skewBps)
        concurrent = String(c.maxConcurrent); failures = String(c.maxConsecutiveFailures); replacements = String(c.maxReplacementFailures); failureRate = String(c.failureRateBps)
    }
    func edit(context: TradeContext) throws -> Blakeswap_V2_StrategyEdit {
        let a = try common.edit(context: context).config
        var c = Blakeswap_V2_StrategyConfig()
        c.id = id; c.wallet = context.profile; c.network = context.network
        c.btc = try btc.value(); c.blake = try blake.value()
        c.rate = a.rate; c.minRate = a.minRate; c.maxRate = a.maxRate
        c.spreadBps = try strategyInteger(spread); c.minSpreadBps = try strategyInteger(minSpread); c.maxSpreadBps = try strategyInteger(maxSpread); c.skewBps = try strategyInteger(skew)
        c.lifetime = a.lifetime; c.cadence = a.cadence
        c.btcFeeBudget = a.btcFeeBudget; c.blakeFeeBudget = a.blakeFeeBudget
        c.towerBps = a.towerBps; c.towerPubkey = a.towerPubkey
        c.reference = a.reference; c.referenceMakers = a.referenceMakers; c.referenceFreshness = a.referenceFreshness; c.referenceSpreadBps = a.referenceSpreadBps
        guard let count = UInt32(concurrent), let failed = UInt32(failures), let replaced = UInt32(replacements) else { throw RPCError.message("Concurrency and failure limits must be whole numbers.") }
        c.maxConcurrent = count; c.maxConsecutiveFailures = failed; c.maxReplacementFailures = replaced; c.failureRateBps = try strategyInteger(failureRate)
        var q = Blakeswap_V2_StrategyEdit(); q.config = c; q.expectedRevision = common.revision; q.enabled = common.enabled; q.acknowledgeRestoredBudget = common.acknowledgeRestored
        return q
    }
}

@MainActor
final class StrategyModel: ObservableObject {
    @Published private(set) var strategies: [Blakeswap_V2_StrategyView] = []
    @Published private(set) var busy = false
    @Published private(set) var loaded = false
    @Published private(set) var error: String?
    @Published private(set) var review: Blakeswap_V2_StrategyReview?
    private var reviewedEdit: Blakeswap_V2_StrategyEdit?
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
            let data = try await call("strategy.list", q.jsonUTF8Data())
            let result = try Blakeswap_V2_StrategyList(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard result.wallet == context.profile, result.network == context.network, result.strategies.allSatisfy({ $0.config.wallet == context.profile && $0.config.network == context.network }) else { throw RPCError.message("Strategy wallet or network changed.") }
            strategies = result.strategies; loaded = true
        } catch { if context.matches(current()) { self.error = error.localizedDescription } }
    }
    func report(_ strategy: Blakeswap_V2_StrategyView, current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil; defer { busy = false }
        do {
            var q = Blakeswap_V2_StrategyReportRequest(); q.id = strategy.id; q.expectedWallet = context.profile; q.expectedNetwork = context.network; q.expectedRevision = strategy.revision
            let data = try await call("strategy.report", q.jsonUTF8Data())
            let r = try Blakeswap_V2_StrategyView(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard r.id == strategy.id, r.revision == strategy.revision, r.config.wallet == context.profile, r.config.network == context.network, r.reportIncluded else { throw RPCError.message("Report changed; refresh current strategy.") }
            if let i = strategies.firstIndex(where: { $0.id == r.id && $0.revision == r.revision }) { strategies[i] = r }
        } catch { if context.matches(current()) { self.error = error.localizedDescription } }
    }
    func review(_ draft: StrategyDraft, current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil; review = nil; reviewedEdit = nil; defer { busy = false }
        do {
            var q = try draft.edit(context: context)
            let data = try await call("strategy.review", q.jsonUTF8Data())
            let r = try Blakeswap_V2_StrategyReview(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard r.config == q.config, r.expectedRevision == q.expectedRevision, r.enabled == q.enabled, !r.reviewDigest.isEmpty else { throw RPCError.message("Authorization changed; review the complete strategy again.") }
            q.reviewDigest = r.reviewDigest; reviewedEdit = q; review = r
        } catch { if context.matches(current()) { self.error = error.localizedDescription } }
    }
    func revise() { review = nil; reviewedEdit = nil; error = nil }
    func save(current: () -> TradeContext) async -> Bool {
        guard !busy, context.matches(current()), let q = reviewedEdit else { return false }
        busy = true; error = nil; defer { busy = false }
        do {
            let data = try await call("strategy.save", q.jsonUTF8Data())
            let r = try Blakeswap_V2_StrategyView(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()), r.config == q.config, r.revision == q.expectedRevision + 1, r.enabled == q.enabled else { throw RPCError.message("Strategy save changed; refresh before retrying.") }
            revise(); return true
        } catch { if context.matches(current()) { self.error = "\(error.localizedDescription) Refresh to resolve an uncertain save; retained strategy IDs prevent duplicate authorizations." }; return false }
    }
    func stop(_ p: Blakeswap_V2_StrategyView, permanent: Bool, current: () -> TradeContext) async {
        guard !busy, context.matches(current()) else { return }
        busy = true; error = nil
        do {
            var q = Blakeswap_V2_StopStrategyRequest(); q.id = p.id; q.expectedWallet = context.profile; q.expectedNetwork = context.network; q.expectedRevision = p.revision; q.stop = permanent
            _ = try await call("strategy.stop", q.jsonUTF8Data()); busy = false; await load(current: current)
        } catch { busy = false; if context.matches(current()) { self.error = error.localizedDescription } }
    }
}

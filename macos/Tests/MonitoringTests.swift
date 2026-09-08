import Foundation
import XCTest
@testable import Blakeswap

@MainActor final class RecordingAlerts: AlertDelivery {
    var allowed = true
    var delivered: [(String, String)] = []
    var removed: [String] = []
    func permission(request: Bool) async -> Bool { allowed }
    func deliver(id: String, body: String) async throws { delivered.append((id, body)) }
    func remove(ids: [String]) { removed += ids }
}
final class MonitoringTests: XCTestCase {
    private func summary(now: Int64, state: String = "confirming", firstReveal: Bool = false) -> ActionSummary {
        var action = WalletAction(); action.id = "swap/private-local-id"; action.kind = "swap"; action.objectID = "private-local-id"
        action.state = state; action.requiresMonitoring = state != "completed"; action.firstReveal = firstReveal
        var wallet = Blakeswap_V2_WalletActions(); wallet.walletID = "other-wallet"; wallet.network = "regtest"; wallet.known = true; wallet.observedAt = now; wallet.source = "live"; wallet.actions = [action]
        var summary = ActionSummary(); summary.network = "regtest"; summary.settingsRevision = 1; summary.observedAt = now; summary.complete = true; summary.requiresMonitoring = action.requiresMonitoring; summary.wallets = [wallet]
        return summary
    }
    @MainActor func testPrivateDedupAcrossRestartPreferencesAndPermissionDenial() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); let now: Int64 = 1000
        var model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        let value = summary(now: now, firstReveal: true)
        await model.reconcile(value); await model.reconcile(value)
        XCTAssertEqual(delivery.delivered.count, 1)
        XCTAssertTrue(delivery.delivered[0].1.contains("external tower cannot"))
        XCTAssertFalse(delivery.delivered[0].1.contains("other-wallet")); XCTAssertFalse(delivery.delivered[0].1.contains("private-local-id"))
        model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        await model.reconcile(value); XCTAssertEqual(delivery.delivered.count, 1)
        var prefs = model.preferences; prefs.completed = false; model.setPreferences(prefs)
        await model.reconcile(summary(now: now, state: "completed")); XCTAssertEqual(delivery.delivered.count, 1)
        prefs.completed = true; model.setPreferences(prefs)
        await model.reconcile(summary(now: now, state: "completed")); XCTAssertEqual(delivery.delivered.count, 1)
        delivery.allowed = false
        await model.reconcile(summary(now: now, state: "owner_claim"))
        delivery.allowed = true
        await model.reconcile(summary(now: now, state: "owner_claim")); XCTAssertEqual(delivery.delivered.count, 1, "Denied events must not burst when permission changes")
        let journal = root.appendingPathComponent("notifications/journal.json")
        let attributes = try FileManager.default.attributesOfItem(atPath: journal.path)
        XCTAssertEqual((attributes[.posixPermissions] as? NSNumber)?.intValue, 0o600)
    }
    @MainActor func testTerminalBaselineReorgReopensWithoutObsoleteSuccess() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); let now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        await model.reconcile(summary(now: now, state: "completed")); XCTAssertTrue(delivery.delivered.isEmpty)
        await model.reconcile(summary(now: now, state: "confirming"))
        XCTAssertEqual(delivery.delivered.count, 1)
        await model.reconcile(summary(now: now, state: "confirming"))
        XCTAssertEqual(delivery.delivered.count, 1, "Reopened transition must not generate a second generic transition")
        await model.reconcile(summary(now: now, state: "completed"))
        XCTAssertEqual(delivery.delivered.count, 1, "A reorg must not replay its obsolete success")
        XCTAssertFalse(delivery.removed.isEmpty)
    }
    @MainActor func testWakeWaitsForFreshClockAndRecomputesDeadline() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); var now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        var value = summary(now: now, firstReveal: true)
        var deadline = Blakeswap_V2_ActionDeadline(); deadline.kind = "reveal"; deadline.chain = "btc"; deadline.unit = "blocks"; deadline.target = 120; deadline.observed = 116; deadline.remaining = 4; deadline.observedAt = now; deadline.certain = true; deadline.band = "approaching"
        value.wallets[0].actions[0].deadlines = [deadline]
        model.interruption(); await model.reconcile(value)
        XCTAssertTrue(model.interrupted); XCTAssertTrue(delivery.delivered.isEmpty)
        now += 5; value.observedAt = now; value.wallets[0].observedAt = now
        value.wallets[0].actions[0].deadlines[0].observedAt = now
        await model.reconcile(value)
        XCTAssertFalse(model.interrupted); XCTAssertEqual(delivery.delivered.count, 2)
        await model.reconcile(value); XCTAssertEqual(delivery.delivered.count, 2)
        value.wallets[0].actions[0].deadlines[0].certain = false
        value.wallets[0].actions[0].deadlines[0].band = "unknown"
        XCTAssertTrue(value.wallets[0].actions[0].deadlines[0].display.contains("timing unavailable"))
        await model.reconcile(value); XCTAssertEqual(delivery.delivered.count, 2)
    }
    @MainActor func testStoredTerminalCannotEmitNewSuccessAndUnsafeJournalRefused() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); let now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        var value = summary(now: now, state: "completed"); value.wallets[0].source = "stored"
        await model.reconcile(value); XCTAssertTrue(delivery.delivered.isEmpty)
        try AlertStore(root: root.path).save(AlertJournal())
        let file = root.appendingPathComponent("notifications/journal.json")
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        let broken = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        await broken.reconcile(summary(now: now)); XCTAssertNotNil(broken.error); XCTAssertTrue(delivery.delivered.isEmpty)
    }
}

extension MonitoringTests {
    @MainActor func testInterruptedUnavailableAndExpiredSnapshotsInvalidateDisplayedTiming() async throws {
        for change in ["sleep", "unavailable", "expired"] {
            let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
            let delivery = RecordingAlerts(); var now: Int64 = 1000
            let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
            var value = summary(now: now, firstReveal: true)
            var deadline = Blakeswap_V2_ActionDeadline(); deadline.kind = "reveal"; deadline.chain = "btc"; deadline.unit = "blocks"; deadline.observedAt = now; deadline.certain = true; deadline.band = "approaching"; deadline.remaining = 6; deadline.target = 106
            value.wallets[0].actions[0].deadlines = [deadline]
            await model.reconcile(value)
            XCTAssertTrue(model.summary!.wallets[0].actions[0].deadlines[0].display.contains("6 blocks"))
            now += 100
            switch change {
            case "sleep": model.interruption()
            case "unavailable": model.unavailable()
            default: await model.reconcile(value)
            }
            XCTAssertTrue(model.interrupted)
            XCTAssertTrue(model.summary!.wallets[0].actions[0].deadlines[0].display.contains("timing unavailable"), change)
            // A fresh wallet regains timing independently of an offline peer wallet.
            now += 1; value.observedAt = now; value.wallets[0].observedAt = now; value.wallets[0].actions[0].deadlines[0].observedAt = now
            var offline = Blakeswap_V2_WalletActions(); offline.walletID = "offline"; offline.known = false
            value.wallets.append(offline); value.complete = false
            await model.reconcile(value)
            XCTAssertTrue(model.interrupted)
            XCTAssertTrue(model.summary!.wallets[0].actions[0].deadlines[0].certain)
            XCTAssertEqual(delivery.delivered.count, 2, "Fresh observations must not replay the already consumed event")
        }
    }
    @MainActor func testDefaultNotificationProviderIsSafeOutsideAppBundle() async throws {
        XCTAssertNotEqual(Bundle.main.bundleURL.pathExtension, "app")
        let provider = SystemAlertDelivery()
        let allowed = await provider.permission(request: false)
        let requested = await provider.permission(request: true)
        XCTAssertFalse(allowed); XCTAssertFalse(requested)
        provider.remove(ids: []); provider.remove(ids: ["obsolete"])
        do { try await provider.deliver(id: "private-id", body: "Private message"); XCTFail("Unbundled delivery unexpectedly succeeded") }
        catch { XCTAssertTrue(error.localizedDescription.contains("app bundle")) }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let model = MonitoringModel(root: root.path, now: { 1000 })
        await model.reconcile(summary(now: 1000)); XCTAssertFalse(model.permissionGranted); XCTAssertNil(model.error)
    }
    @MainActor func testInterruptionStopsRemainingSuspendedDeliveryBatch() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = SuspendedAlerts(); var now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        var value = summary(now: now, firstReveal: true)
        var deadline = Blakeswap_V2_ActionDeadline(); deadline.kind = "reveal"; deadline.chain = "btc"; deadline.observedAt = now; deadline.certain = true; deadline.band = "approaching"; deadline.target = 120
        value.wallets[0].actions[0].deadlines = [deadline]
        let pending = Task { await model.reconcile(value) }
        await delivery.waitForSubmission()
        now += 1; model.interruption()
        delivery.resume(); await pending.value
        XCTAssertEqual(delivery.delivered.count, 1, "A suspended old batch must not submit its deadline after monitoring is interrupted")
        XCTAssertTrue(model.interrupted)
    }
    @MainActor func testSuspendedBatchHonorsPreferencesNewSummaryAndObservationAge() async throws {
        for change in ["preferences", "summary", "age"] {
            let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
            let delivery = SuspendedAlerts(); var now: Int64 = 1000
            let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
            var value = summary(now: now, firstReveal: true)
            var deadline = Blakeswap_V2_ActionDeadline(); deadline.kind = "reveal"; deadline.chain = "btc"; deadline.observedAt = now; deadline.certain = true; deadline.band = "approaching"; deadline.target = 120
            value.wallets[0].actions[0].deadlines = [deadline]
            let pending = Task { await model.reconcile(value) }
            await delivery.waitForSubmission()
            switch change {
            case "preferences": var prefs = model.preferences; prefs.deadlines = false; model.setPreferences(prefs)
            case "summary":
                await model.reconcile(summary(now: now, state: "completed"))
                XCTAssertEqual(model.summary?.wallets[0].actions[0].state, "completed")
            default: now += 91
            }
            delivery.resume(); await pending.value
            XCTAssertEqual(delivery.delivered.count, 1, "Obsolete batch continued after \(change)")
        }
    }
    @MainActor func testFreshWalletDeadlineSurvivesOtherWalletOutageAfterWake() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); var now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        model.interruption(); now += 10
        var value = summary(now: now, firstReveal: true)
        var deadline = Blakeswap_V2_ActionDeadline(); deadline.kind = "reveal"; deadline.chain = "btc"; deadline.unit = "blocks"; deadline.observedAt = now; deadline.certain = true; deadline.band = "approaching"; deadline.remaining = 2; deadline.target = 120
        value.wallets[0].actions[0].deadlines = [deadline]
        var offline = Blakeswap_V2_WalletActions(); offline.walletID = "offline"; offline.known = false
        value.wallets.append(offline); value.complete = false
        await model.reconcile(value)
        XCTAssertTrue(model.interrupted, "The installation still has interrupted monitoring")
        XCTAssertEqual(delivery.delivered.count, 2, "Healthy wallet transition and fresh deadline must not be suppressed by another wallet")
        await model.reconcile(value); XCTAssertEqual(delivery.delivered.count, 2)
    }
    @MainActor func testDurableReorgAttentionDoesNotRequireCertainDeadline() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); let now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        await model.reconcile(summary(now: now, state: "completed"))
        var reopened = summary(now: now, state: "reopened"); reopened.wallets[0].actions[0].uncertain = true
        await model.reconcile(reopened)
        XCTAssertEqual(delivery.delivered.count, 1, "A known durable reorg hold is actionable even before fresh settlement evidence returns")
        await model.reconcile(reopened); XCTAssertEqual(delivery.delivered.count, 1)
    }
}

@MainActor final class SuspendedAlerts: AlertDelivery {
    var delivered: [String] = []
    private var continuation: CheckedContinuation<Void, Never>?
    private var started: CheckedContinuation<Void, Never>?
    func permission(request: Bool) async -> Bool { true }
    func deliver(id: String, body: String) async throws {
        delivered.append(id)
        if delivered.count == 1 {
            await withCheckedContinuation { continuation = $0; started?.resume(); started = nil }
        }
    }
    func waitForSubmission() async {
        if continuation != nil { return }
        await withCheckedContinuation { started = $0 }
    }
    func resume() { continuation?.resume(); continuation = nil }
    func remove(ids: [String]) {}
}

extension MonitoringTests {
    @MainActor func testCertainTargetDeadlineAlertsDuringPeerClockOutage() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let delivery = RecordingAlerts(); let now: Int64 = 1000
        let model = MonitoringModel(root: root.path, delivery: delivery, now: { now })
        var value = summary(now: now)
        value.wallets[0].actions[0].uncertain = true
        var target = Blakeswap_V2_ActionDeadline(); target.kind = "refund"; target.chain = "btc"; target.unit = "blocks"; target.observedAt = now; target.certain = true; target.band = "approaching"; target.remaining = 2; target.target = 120
        var reveal = target; reveal.kind = "reveal"; reveal.certain = false; reveal.band = "unknown"
        value.wallets[0].actions[0].deadlines = [target, reveal]
        await model.reconcile(value)
        XCTAssertEqual(delivery.delivered.count, 1, "Fresh own-chain deadline may alert while cross-chain revelation remains uncertain")
        XCTAssertTrue(delivery.delivered.first?.1.contains("contract deadline") == true)
        await model.reconcile(value); XCTAssertEqual(delivery.delivered.count, 1)
    }
}

extension MonitoringTests {
    func testJournalCapacityRetainsDedupAcrossRestartAndChecksBytesBeforeDecode() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let small = AlertStore(root: root.path, maximumEntries: 2, maximumBytes: 4096)
        var journal = AlertJournal(); journal.seen = ["first-success", "second-success"]
        try small.save(journal)
        var overflow = journal; overflow.seen.insert("third")
        XCTAssertThrowsError(try small.save(overflow))
        XCTAssertEqual(try small.load().seen, journal.seen, "Capacity cannot evict old success identities")
        let file = root.appendingPathComponent("notifications/journal.json")
        try Data(repeating: 0x78, count: 4097).write(to: file)
        do { _ = try small.load(); XCTFail("Oversized data decoded") } catch { XCTAssertTrue(error.localizedDescription.contains("limit")) }
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        XCTAssertThrowsError(try small.load())
    }
    func testJournalSupportsMoreThanThousandHistoricalEvents() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString); defer { try? FileManager.default.removeItem(at: root) }
        let store = AlertStore(root: root.path)
        var journal = AlertJournal()
        for i in 0..<5000 { journal.seen.insert(String(format: "%064x", i)) }
        try store.save(journal)
        XCTAssertEqual(try store.load().seen, journal.seen)
    }
}

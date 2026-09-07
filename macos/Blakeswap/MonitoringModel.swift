import AppKit
import CryptoKit
import Foundation
import SwiftUI
import UserNotifications

typealias ActionSummary = Blakeswap_V1_ActionSummary
typealias WalletAction = Blakeswap_V1_WalletAction

struct AlertPreferences: Codable, Equatable {
    var transitions = true
    var deadlines = true
    var attention = true
    var completed = true
    func permits(_ category: String) -> Bool {
        switch category { case "deadline": return deadlines; case "attention": return attention; case "completed": return completed; default: return transitions }
    }
}
struct AlertDestination: Codable, Equatable { let network: String; let wallet: String; let kind: String; let object: String }
struct AlertJournal: Codable {
    var preferences = AlertPreferences()
    var seen = Set<String>()
    var states: [String: String] = [:]
    var routes: [String: AlertDestination] = [:]
    var initialized = Set<String>()
}
struct AlertStore {
    let root: String
    private var directory: URL { URL(fileURLWithPath: root).appendingPathComponent("notifications", isDirectory: true) }
    private var path: URL { directory.appendingPathComponent("journal.json") }
    private func check(_ url: URL, directory: Bool = false) throws {
        let a = try FileManager.default.attributesOfItem(atPath: url.path)
        guard a[.type] as? FileAttributeType == (directory ? .typeDirectory : .typeRegular), let mode = a[.posixPermissions] as? NSNumber, mode.intValue & 0o077 == 0 else { throw RPCError.message("Notification preferences and history must be private files.") }
    }
    func load() throws -> AlertJournal {
        if !FileManager.default.fileExists(atPath: directory.path) { return AlertJournal() }
        try check(directory, directory: true)
        if !FileManager.default.fileExists(atPath: path.path) { return AlertJournal() }
        try check(path)
        return try JSONDecoder().decode(AlertJournal.self, from: Data(contentsOf: path))
    }
    func save(_ journal: AlertJournal) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try check(directory, directory: true)
        if FileManager.default.fileExists(atPath: path.path) { try check(path) }
        let temporary = directory.appendingPathComponent(UUID().uuidString)
        guard FileManager.default.createFile(atPath: temporary.path, contents: try JSONEncoder().encode(journal), attributes: [.posixPermissions: 0o600]) else { throw RPCError.message("Cannot save notification history.") }
        defer { try? FileManager.default.removeItem(at: temporary) }
        guard rename(temporary.path, path.path) == 0 else { throw RPCError.message("Cannot save notification history.") }
    }
}

@MainActor protocol AlertDelivery: AnyObject {
    func permission(request: Bool) async -> Bool
    func deliver(id: String, body: String) async throws
    func remove(ids: [String])
}
@MainActor final class SystemAlertDelivery: NSObject, AlertDelivery, UNUserNotificationCenterDelegate {
    var navigate: ((String) -> Void)?
    private var center: UNUserNotificationCenter { .current() }
    func permission(request: Bool) async -> Bool {
        center.delegate = self
        if request { _ = try? await center.requestAuthorization(options: [.alert, .sound]) }
        let status = await center.notificationSettings().authorizationStatus
        return status == .authorized || status == .provisional
    }
    func deliver(id: String, body: String) async throws {
        let content = UNMutableNotificationContent()
        content.title = "Blakeswap"; content.body = body
        // The opaque identifier resolves through our private journal. No wallet,
        // order/transaction ID, address, amount or secret enters lock-screen text.
        try await center.add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }
    func remove(ids: [String]) { center.removePendingNotificationRequests(withIdentifiers: ids); center.removeDeliveredNotifications(withIdentifiers: ids) }
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        await MainActor.run { self.navigate?(response.notification.request.identifier) }
    }
}

extension WalletAction {
    var monitoringTitle: String {
        switch state {
        case "completed": return kind == "tower" ? "Rescue confirmed" : "Settlement confirmed"
        case "refunded": return "Refund confirmed"
        case "closed": return "Closed"
        case "reopened": return "Settlement reopened after a chain change"
        case "attention": return "Broadcast or monitoring needs attention"
        case "owner_claim": return "Owner claim in progress"
        case "owner_refund": return "Monitoring the refund deadline"
        case "signed_pending": return "Signed send awaits publication"
        case "confirming": return "Funding or settlement awaits confirmations"
        case "tower_pending": return "Waiting for durable tower protection"
        case "tower_monitoring": return "Accepted local rescue job needs monitoring"
        case "offer_open": return "Open order can accept a trade"
        case "recovery_required": return "Restored obligations need reconciliation"
        default: return "Waiting for the peer"
        }
    }
}
extension Blakeswap_V1_ActionDeadline {
    var display: String {
        let label = kind.replacingOccurrences(of: "_", with: " ").capitalized
        guard certain else { return "\(chain.uppercased()) \(label): timing unavailable · last observation \(observedAt > 0 ? Date(timeIntervalSince1970: Double(observedAt)).formatted() : "unknown")" }
        if unit == "blocks" { return "\(chain.uppercased()) \(label): \(max(0, remaining)) blocks remaining (height \(observed), threshold \(target))" }
        return "\(chain.uppercased()) \(label): \(max(0, remaining)) median-time seconds remaining · chain MTP \(Date(timeIntervalSince1970: Double(observed)).formatted()) · threshold \(Date(timeIntervalSince1970: Double(target)).formatted()). Calendar time is an estimate; chain MTP controls eligibility."
    }
}

@MainActor final class MonitoringModel: ObservableObject {
    @Published private(set) var summary: ActionSummary?
    @Published private(set) var interrupted = false
    @Published private(set) var permissionGranted = false
    @Published private(set) var error: String?
    @Published private(set) var preferences = AlertPreferences()
    var navigate: ((AlertDestination) -> Void)?
    private let store: AlertStore
    private let delivery: AlertDelivery
    private let now: () -> Int64
    private var journal = AlertJournal()
    private var journalReady = false
    private var processing = false
    private var interruptionTime: Int64 = 0
    init(root: String, delivery: AlertDelivery? = nil, now: @escaping () -> Int64 = { Int64(Date().timeIntervalSince1970) }) {
        store = AlertStore(root: root); self.delivery = delivery ?? SystemAlertDelivery(); self.now = now
        do { journal = try store.load(); preferences = journal.preferences; journalReady = true } catch { self.error = error.localizedDescription }
        (self.delivery as? SystemAlertDelivery)?.navigate = { [weak self] id in
            guard let self, let route = self.journal.routes[id] else { return }; self.navigate?(route)
        }
    }
    func setPreferences(_ next: AlertPreferences) {
        guard journalReady else { return }
        var updated = journal; updated.preferences = next
        do { try store.save(updated); journal = updated; preferences = next } catch { self.error = error.localizedDescription }
    }
    func requestPermission() async { permissionGranted = await delivery.permission(request: true) }
    func interruption() { interrupted = true; interruptionTime = now() }
    func unavailable() { interrupted = true }
    private func hash(_ text: String) -> String { SHA256.hash(data: Data(text.utf8)).map { String(format: "%02x", $0) }.joined() }
    func reconcile(_ next: ActionSummary) async {
        guard !processing else { return }; processing = true; defer { processing = false }
        summary = next
        let current = now()
        guard journalReady, next.observedAt <= current, current - next.observedAt <= 90 else { interrupted = true; return }
        let caughtUp = next.complete && next.wallets.allSatisfy { wallet in
            wallet.known && (!interrupted || wallet.source == "stored" || wallet.observedAt > interruptionTime) && wallet.actions.allSatisfy { !$0.requiresMonitoring || (!$0.uncertain && $0.deadlines.allSatisfy { $0.certain && (!interrupted || $0.observedAt > interruptionTime) }) }
        }
        if caughtUp { interrupted = false }
        permissionGranted = await delivery.permission(request: false)
        var updated = journal
        var deliveries: [(String, String)] = []
        var currentEvents = Set<String>()
        for wallet in next.wallets where wallet.known && wallet.source == "live" {
            let scope = next.network + "|" + wallet.walletID
            let initialized = journal.initialized.contains(scope)
            for action in wallet.actions {
                let key = hash(scope + "|" + action.id)
                let previous = journal.states[key]
                var events: [(String, String, String)] = []
                if action.requiresMonitoring {
                    let state = action.state + "|" + String(action.towerReady) + "|" + String(action.firstReveal)
                    let reopened = previous == "completed" || previous == "refunded"
                    if reopened { updated.seen.insert(hash(key + "|" + state)) }
                    events.append((reopened ? "reopened" : state, reopened || action.state == "attention" ? "attention" : "transition", action.firstReveal ? "A swap needs this app for its first secret revelation. An external tower cannot perform it." : action.monitoringTitle + ". Open Blakeswap to review."))
                    for deadline in action.deadlines where deadline.certain && ["approaching", "reached"].contains(deadline.band) {
                        events.append((deadline.kind + "|" + deadline.chain + "|" + String(deadline.target) + "|" + deadline.band, "deadline", deadline.kind == "reveal" ? "A first-revelation cutoff is near or reached. Keep Blakeswap open and review the swap." : "A contract deadline needs monitoring. Open Blakeswap to review."))
                    }
                } else if ["completed", "refunded"].contains(action.state) {
                    events.append((action.state, "completed", "A settlement was confirmed. Open Blakeswap to review."))
                }
                updated.states[key] = action.state
                for (event, category, body) in events {
                    let id = hash(key + "|" + event)
                    currentEvents.insert(id)
                    updated.routes[id] = AlertDestination(network: next.network, wallet: wallet.walletID, kind: action.kind, object: action.objectID)
                    if action.uncertain || interrupted { continue }
 guard updated.seen.insert(id).inserted else { continue }
                    // Establish an initial terminal baseline without replaying old
                    // success. Suppressed/denied events are consumed, never burst later.
                    if category == "completed" && (!initialized || previous == nil) { continue }
                    if !action.uncertain && !interrupted && preferences.permits(category) && permissionGranted { deliveries.append((id, body)) }
                }
            }
            updated.initialized.insert(scope)
        }
        do {
            try store.save(updated) // Persist before submitting to avoid crash/restart duplicates.
            let obsolete = journal.routes.keys.filter { !currentEvents.contains($0) }
            journal = updated
            delivery.remove(ids: obsolete)
            for (id, body) in deliveries { try await delivery.deliver(id: id, body: body) }
        } catch { self.error = error.localizedDescription }
    }
}

struct MonitoringView: View {
    @ObservedObject var model: MonitoringModel
    let open: (AlertDestination) -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Monitoring across wallets").font(.headline)
            if model.interrupted { Text("Monitoring was interrupted. Rechecking current chain observations; timing may be unavailable.").foregroundStyle(.orange) }
            if let summary = model.summary {
                ForEach(summary.wallets, id: \.walletID) { wallet in
                    if !wallet.known { Text("\(wallet.walletID): local obligation state is being checked. Quitting will stop that check.").foregroundStyle(.orange) }
                    ForEach(wallet.actions.filter(\.requiresMonitoring), id: \.id) { action in
                        Button { open(AlertDestination(network: summary.network, wallet: wallet.walletID, kind: action.kind, object: action.objectID)) } label: {
                            VStack(alignment: .leading, spacing: 4) {
                                Text("\(wallet.walletID): \(action.monitoringTitle)")
                                if action.firstReveal { Text("This app must perform the taker’s first revelation, even when a tower is armed.").foregroundStyle(.orange) }
                                if action.towerReady { Text("Tower protection recorded · it cannot perform the first revelation.") }
                                if action.uncertain { Text("Current chain eligibility is uncertain.").foregroundStyle(.orange) }
                                ForEach(Array(action.deadlines.enumerated()), id: \.offset) { _, deadline in Text(deadline.display) }
                            }.font(.caption).frame(maxWidth: .infinity, alignment: .leading)
                        }.buttonStyle(.plain)
                    }
                }
                if summary.complete && !summary.requiresMonitoring { Text("No recorded obligations currently require continued monitoring.").font(.caption) }
            } else { Text("Checking all saved wallets…").font(.caption) }
            Text("Quitting stops the app-owned daemon. Notifications do not monitor chains while the app is closed.").font(.caption).foregroundStyle(.secondary)
        }.padding().background(panel, in: RoundedRectangle(cornerRadius: 12))
    }
}
struct NotificationPreferencesView: View {
    @ObservedObject var model: MonitoringModel
    private func preference(_ key: WritableKeyPath<AlertPreferences, Bool>) -> Binding<Bool> {
        Binding(get: { model.preferences[keyPath: key] }, set: { value in var p = model.preferences; p[keyPath: key] = value; model.setPreferences(p) })
    }
    var body: some View {
        GroupBox("Private notifications") {
            VStack(alignment: .leading, spacing: 8) {
                Toggle("Trade, funding and tower transitions", isOn: preference(\.transitions))
                Toggle("Approaching contract deadlines", isOn: preference(\.deadlines))
                Toggle("Failed broadcasts and reopened settlements", isOn: preference(\.attention))
                Toggle("Confirmed settlements", isOn: preference(\.completed))
                Button("Enable macOS notifications") { Task { await model.requestPermission() } }
                Text(model.permissionGranted ? "Notifications allowed. Lock-screen messages omit wallet details and amounts." : "Notifications are unavailable or permission has not been granted. In-app monitoring and quit protection remain active.").font(.caption)
                if let error = model.error { Text(error).font(.caption).foregroundStyle(.orange) }
            }.padding(8)
        }
    }
}

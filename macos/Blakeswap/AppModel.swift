import AppKit
import Foundation
import SwiftUI
import SwiftProtobuf

@MainActor
final class AppModel: ObservableObject {
    @Published var profile = "alice"
    @Published var page = "Market"
    struct Snapshot {
        var status: DaemonStatus?
        var settings: AppSettings?
    }
    @Published private(set) var snapshot = Snapshot()
    var status: DaemonStatus? { snapshot.status }
    var settings: AppSettings? { snapshot.settings }
    private(set) var generation: UInt64 = 0
    @Published var connectionError: String?
    @Published var notice: String?
    @Published var busy = false
    @Published var recovery: String?
    let root = DaemonProcess.shared.root
    private var refreshing = false
    var network: String { settings?.activeNetwork ?? status?.network ?? "mainnet" }
    var isRegtest: Bool { network == "regtest" }
    func invalidateSnapshot() { generation &+= 1; snapshot.status = nil; recovery = nil }
    func selectProfile(_ name: String) { invalidateSnapshot(); profile = name; notice = nil }

    @discardableResult
    func acceptSnapshot(_ next: DaemonStatus?, settings nextSettings: AppSettings, profile selected: String, generation expected: UInt64) -> Bool {
        guard expected == generation, selected == profile,
              nextSettings.revision >= (settings?.revision ?? 0) else { return false }
        if nextSettings.revision != settings?.revision || nextSettings.activeNetwork != settings?.activeNetwork {
            generation &+= 1
            recovery = nil
        }
        let matching = next?.network == nextSettings.activeNetwork && next?.name == selected
        snapshot = Snapshot(status: matching ? next : nil, settings: nextSettings)
        if nextSettings.activeNetwork != "regtest" && profile != "alice" { selectProfile("alice") }
        return matching
    }
    func start() { do { try DaemonProcess.shared.start() } catch { connectionError = error.localizedDescription } }
    func refresh() async {
        guard !refreshing else { return }; refreshing = true; defer { refreshing = false }
        if let failure = DaemonProcess.shared.failure { connectionError = failure; return }
        let selected = profile, expected = generation
        do {
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "status")
            let next = try DaemonStatus(serializedBytes: raw)
            let settingsRaw = try await DaemonRPC.call(root: root, profile: selected, method: "settings.get")
            let nextSettings = try AppSettings(serializedBytes: settingsRaw)
            if acceptSnapshot(next, settings: nextSettings, profile: selected, generation: expected) { connectionError = nil }
        } catch { if selected == profile && expected == generation { connectionError = error.localizedDescription } }
    }
    func loadSettings() async {
        let selected = profile, expected = generation
        do {
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "settings.get")
            let next = try AppSettings(serializedBytes: raw)
            acceptSnapshot(status, settings: next, profile: selected, generation: expected)
        } catch { if selected == profile && expected == generation { notice = error.localizedDescription } }
    }
    func saveSettings(_ draft: AppSettings) async {
        guard !busy else { return }; busy = true; invalidateSnapshot()
        defer { busy = false; invalidateSnapshot() }
        do {
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "settings.update", payload: draft.jsonUTF8Data())
            let next = try AppSettings(serializedBytes: raw)
            acceptSnapshot(nil, settings: next, profile: profile, generation: generation)
            notice = "Settings saved. Connecting."
        } catch { notice = error.localizedDescription }
    }
    func checkNode(network: String, chain: String, node: NodeSettings) async -> String {
        do {
            var request = Blakeswap_V1_CheckNodeRequest(); request.network = network; request.chain = chain; request.node = node
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "settings.check-node", payload: request.jsonUTF8Data())
            let result = try Blakeswap_V1_CheckNodeResponse(serializedBytes: raw)
            return "Connected at block \(result.height). \(result.trust)"
        } catch { return error.localizedDescription }
    }
    func command(_ method: String, _ params: [String: Any] = [:]) async -> Bool {
        guard !busy else { return false }; busy = true; notice = nil
        let selected = profile
        defer { busy = false }
        do {
            var bound = params
            if ["offer.create", "offer.cancel", "swap.take", "pause", "regtest.mine", "regtest.faucet"].contains(method) {
                bound["expected_network"] = status?.network ?? network
            }
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: method, params: bound)
            if selected == profile {
                if method == "wallet.recovery" { recovery = try Blakeswap_V1_Recovery(serializedBytes: raw).mnemonic }
                if method == "wallet.backup" {
                    let path = try Blakeswap_V1_Backup(serializedBytes: raw).path
                    notice = "Backup saved. Keep the vault password separately."
                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                }
                if method == "swap.take" { page = "Swaps" }
                if method == "offer.create" { notice = "Offer queued." }
            }
            await refresh(); return true
        } catch { notice = error.localizedDescription; return false }
    }
}

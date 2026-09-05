import AppKit
import Foundation
import SwiftUI
import SwiftProtobuf

@MainActor
final class AppModel: ObservableObject {
    @Published var profile = "alice"
    @Published var page = "Market"
    @Published var status: DaemonStatus?
    @Published var settings: AppSettings?
    @Published var connectionError: String?
    @Published var notice: String?
    @Published var busy = false
    @Published var recovery: String?
    let root = DaemonProcess.shared.root
    private var refreshing = false
    var network: String { settings?.activeNetwork ?? status?.network ?? "mainnet" }
    var isRegtest: Bool { network == "regtest" }
    func selectProfile(_ name: String) { profile = name; status = nil; notice = nil; recovery = nil }
    func start() { do { try DaemonProcess.shared.start() } catch { connectionError = error.localizedDescription } }
    func refresh() async {
        guard !refreshing else { return }; refreshing = true; defer { refreshing = false }
        if let failure = DaemonProcess.shared.failure { connectionError = failure; return }
        let selected = profile
        do {
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "status")
            let next = try DaemonStatus(serializedBytes: raw)
            guard selected == profile else { return }
            status = next; connectionError = nil
            await loadSettings()
            if !isRegtest && profile != "alice" { selectProfile("alice") }
        } catch { if selected == profile { connectionError = error.localizedDescription } }
    }
    func loadSettings() async {
        do {
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "settings.get")
            let next = try AppSettings(serializedBytes: raw)
            if settings?.activeNetwork != next.activeNetwork { recovery = nil }
            settings = next
        } catch { notice = error.localizedDescription }
    }
    func saveSettings(_ draft: AppSettings) async {
        guard !busy else { return }; busy = true; defer { busy = false }
        do {
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "settings.update", payload: draft.jsonUTF8Data())
            settings = try AppSettings(serializedBytes: raw)
            if !isRegtest { selectProfile("alice") }
            status = nil; recovery = nil; notice = "Settings saved. Connecting."
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

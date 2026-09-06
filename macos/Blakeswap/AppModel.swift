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
    @Published var setupWallet: Blakeswap_V1_FirstWallet?
    private let daemon: DaemonProcess
    let root: String
    init(daemon: DaemonProcess? = nil) {
        self.daemon = daemon ?? .shared
        root = self.daemon.root
    }
    private var refreshing = false
    @Published private(set) var swapRefreshGeneration: UInt64?
    var checkingSwaps: Bool { swapRefreshGeneration == generation }
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
        if nextSettings.onboardingStage != "backup" { setupWallet = nil }
        let matching = next?.network == nextSettings.activeNetwork && next?.name == selected
        snapshot = Snapshot(status: matching ? next : nil, settings: nextSettings)
        if !nextSettings.wallets.isEmpty, !nextSettings.wallets.contains(where: { $0.id == profile }) { selectProfile(nextSettings.wallets[0].id) }
        return matching
    }
    func refresh() async {
        guard !refreshing, !checkingSwaps else { return }; refreshing = true; defer { refreshing = false }
        let selected = profile, expected = generation
        do {
            try daemon.start() // Idempotent while running; restarts an exited helper automatically.
            try await daemon.waitUntilReady(profile: selected)
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "status")
            let next = try DaemonStatus(serializedBytes: raw)
            let settingsRaw = try await DaemonRPC.call(root: root, profile: selected, method: "settings.get")
            let nextSettings = try AppSettings(serializedBytes: settingsRaw)
            if acceptSnapshot(next, settings: nextSettings, profile: selected, generation: expected) { connectionError = nil }
        } catch is CancellationError {
            // Closing the app while its helper starts is not a connection failure.
        } catch { if selected == profile && expected == generation { connectionError = error.localizedDescription } }
    }
    func beginSwapRefresh() -> Bool {
        guard !checkingSwaps else { return false }
        generation &+= 1 // Invalidate polling responses that started before this check.
        swapRefreshGeneration = generation
        return true
    }
    func finishSwapRefresh(_ expected: UInt64) {
        if swapRefreshGeneration == expected { swapRefreshGeneration = nil }
    }
    func refreshSwaps() async {
        guard beginSwapRefresh() else { return }
        let selected = profile, expected = generation
        defer { finishSwapRefresh(expected) }
        let currentNetwork = network
        do {
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "status.refresh", params: ["expected_network": currentNetwork])
            let next = try DaemonStatus(serializedBytes: raw)
            guard let currentSettings = settings else { return }
            if acceptSnapshot(next, settings: currentSettings, profile: selected, generation: expected) { connectionError = nil }
        } catch { if selected == profile && expected == generation { connectionError = error.localizedDescription } }
    }

    @discardableResult
    func loadSettings() async -> AppSettings? {
        let selected = profile, expected = generation
        do {
            let raw = try await DaemonRPC.call(root: root, profile: selected, method: "settings.get")
            let next = try AppSettings(serializedBytes: raw)
            guard selected == profile, expected == generation, next.revision >= (settings?.revision ?? 0) else { return nil }
            acceptSnapshot(status, settings: next, profile: selected, generation: expected)
            return next
        } catch { if selected == profile && expected == generation { notice = error.localizedDescription }; return nil }
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
    func createWallet(name: String) async {
        guard !busy, let current = settings else { return }
        busy = true; invalidateSnapshot()
        defer { busy = false }
        do {
            var request = Blakeswap_V1_CreateWalletRequest()
            request.name = name.trimmingCharacters(in: .whitespacesAndNewlines)
            request.revision = current.revision
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "wallet.create", payload: request.jsonUTF8Data())
            let next = try AppSettings(serializedBytes: raw)
            acceptSnapshot(nil, settings: next, profile: profile, generation: generation)
            if let created = next.wallets.last { selectProfile(created.id) }
            notice = "Wallet created. Connecting."
        } catch { notice = error.localizedDescription }
    }
    func setupAction<M: Message>(_ method: String, request: M) async -> Bool {
        guard !busy else { return false }
        busy = true; notice = nil; invalidateSnapshot()
        defer { busy = false }
        do {
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: method, payload: request.jsonUTF8Data())
            if method == "onboarding.prepare" || method == "onboarding.get" {
                let first = try Blakeswap_V1_FirstWallet(serializedBytes: raw)
                acceptSnapshot(nil, settings: first.settings, profile: profile, generation: generation)
                setupWallet = first.settings.onboardingStage == "backup" ? first : nil
            } else if method == "onboarding.export" {
                _ = try Blakeswap_V1_Backup(serializedBytes: raw)
                notice = "Encrypted backup saved. Keep its password separately."
            } else {
                let next = try AppSettings(serializedBytes: raw)
                acceptSnapshot(nil, settings: next, profile: profile, generation: generation)
            }
            connectionError = nil
            return true
        } catch { notice = error.localizedDescription; return false }
    }
    func checkNode(network: String, chain: String, node: NodeSettings) async -> String {
        do {
            var request = Blakeswap_V1_CheckNodeRequest(); request.network = network; request.chain = chain; request.node = node
            let raw = try await DaemonRPC.call(root: root, profile: profile, method: "settings.check-node", payload: request.jsonUTF8Data())
            let result = try Blakeswap_V1_CheckNodeResponse(serializedBytes: raw)
            return "Connected at block \(result.height)"
        } catch { return error.localizedDescription }
    }
    func command(_ method: String, _ params: [String: Any] = [:]) async -> Bool {
        guard !busy else { return false }; busy = true; notice = nil
        let selected = profile
        defer { busy = false }
        do {
            var bound = params
            if ["fee.quote", "transaction.bump", "tower.resolve", "offer.create", "offer.cancel", "swap.take", "pause", "regtest.mine", "regtest.faucet"].contains(method) {
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
                if method == "swap.take" { page = "Swaps"; notice = "Reservation requested. Waiting for the maker to accept before funding." }
                if method == "offer.create" { notice = "Offer queued." }
                if method == "tower.resolve" { notice = "Private watchtower lookup queued. Waiting for its signed quote." }
            }
            await refresh(); return true
        } catch { notice = error.localizedDescription; return false }
    }
}

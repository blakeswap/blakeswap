import AppKit
import Foundation
import SwiftUI

@MainActor
final class AppModel: ObservableObject {
    @Published var profile = "alice"
    @Published var page = "Market"
    @Published var status: DaemonStatus?
    @Published var connectionError: String?
    @Published var notice: String?
    @Published var busy = false
    @Published var recovery: String?
    let root: String
    private let decoder: JSONDecoder = {
        let d = JSONDecoder(); d.keyDecodingStrategy = .convertFromSnakeCase; return d
    }()

    init() {
        let args = CommandLine.arguments
        if let index = args.firstIndex(of: "--workspace"), args.count > index + 1 {
            root = args[index + 1]
        } else if let url = Bundle.main.url(forResource: "workspace", withExtension: "txt"),
                  let text = try? String(contentsOf: url, encoding: .utf8) {
            root = text.trimmingCharacters(in: .whitespacesAndNewlines)
        } else { root = FileManager.default.currentDirectoryPath }
    }

    func selectProfile(_ name: String) {
        profile = name; status = nil; notice = nil; recovery = nil
    }

    func refresh() async {
        let selected = profile
        do {
            let raw = try await UnixRPC.call(socket: "\(root)/.local/\(selected)/daemon.sock", method: "status")
            struct Reply: Decodable { let result: DaemonStatus?; let error: String? }
            let reply = try decoder.decode(Reply.self, from: raw)
            if let error = reply.error { throw RPCError.message(error) }
            guard selected == profile else { return }
            status = reply.result; connectionError = nil
        } catch { if selected == profile { connectionError = error.localizedDescription } }
    }

    func command(_ method: String, _ params: [String: Any] = [:]) async {
        guard !busy else { return }
        busy = true; notice = nil
        let selected = profile
        defer { busy = false }
        do {
            let data = try JSONSerialization.data(withJSONObject: params)
            let raw = try await UnixRPC.call(socket: "\(root)/.local/\(selected)/daemon.sock", method: method, params: data)
            let reply = try JSONSerialization.jsonObject(with: raw) as? [String: Any]
            if let error = reply?["error"] as? String { throw RPCError.message(error) }
            if selected == profile {
                if method == "wallet.recovery", let result = reply?["result"] as? [String: String] { recovery = result["mnemonic"] }
                if method == "wallet.backup", let result = reply?["result"] as? [String: String], let path = result["path"] {
                    notice = "Encrypted backup saved. Keep its vault password separately."
                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
                }
                if method == "swap.take" { page = "Swaps"; notice = "Request encrypted and queued. The maker can respond when their daemon returns." }
                if method == "offer.create" { notice = "Offer signed and queued for both Nostr relays." }
            }
            await refresh()
        } catch { notice = error.localizedDescription }
    }

    func startNetwork() async {
        guard !busy else { return }; busy = true
        defer { busy = false }
        let workspace = root
        do {
            try await Task.detached {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
                process.arguments = ["python3", "\(workspace)/scripts/dev.py", "up"]
                process.currentDirectoryURL = URL(fileURLWithPath: workspace)
                let logURL = URL(fileURLWithPath: "\(workspace)/.local/gui-start.log")
                try FileManager.default.createDirectory(at: logURL.deletingLastPathComponent(), withIntermediateDirectories: true)
                FileManager.default.createFile(atPath: logURL.path, contents: nil)
                let log = try FileHandle(forWritingTo: logURL)
                defer { try? log.close() }
                process.standardOutput = log; process.standardError = log
                try process.run(); process.waitUntilExit()
                guard process.terminationStatus == 0 else { throw RPCError.message("Startup failed. See .local/gui-start.log in the workspace.") }
            }.value
            await refresh()
        } catch { notice = error.localizedDescription }
    }
}

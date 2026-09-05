import AppKit
import Foundation

@MainActor
final class DaemonProcess {
    static let shared = DaemonProcess()
    private var child: Process?
    private var stopping = false
    private let executable: URL?
    private var log: FileHandle?
    let root: String
    init(root: String? = nil, executable: URL? = nil) {
        self.executable = executable
        if let root { self.root = root; return }
        let args = CommandLine.arguments
        if let index = args.firstIndex(of: "--data-dir"), args.count > index + 1 { self.root = args[index + 1] }
        else { self.root = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Blakeswap").path }
    }
    func start() throws {
        if stopping || child?.isRunning == true { return }
        try? log?.close(); log = nil
        guard let helper = executable ?? Bundle.main.resourceURL?.appendingPathComponent("blakeswap") else { throw RPCError.message("App resources are missing.") }
        try FileManager.default.createDirectory(atPath: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let path = "\(root)/desktop.log"
        if !FileManager.default.fileExists(atPath: path) { FileManager.default.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        log = try FileHandle(forWritingTo: URL(fileURLWithPath: path)); try log?.seekToEnd()
        let process = Process()
        process.executableURL = helper
        process.arguments = ["desktop", "--data-dir", root, "--parent-pid", String(ProcessInfo.processInfo.processIdentifier)]
        process.standardOutput = log; process.standardError = log
        try process.run(); child = process
    }
    func stop() async {
        stopping = true
        guard let process = child else { return }
        if process.isRunning { process.terminate() }
        // Wait for the owned helper to release wallet state.
        await Task.detached { process.waitUntilExit() }.value
        try? log?.close(); log = nil; child = nil
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        Task { await DaemonProcess.shared.stop(); sender.reply(toApplicationShouldTerminate: true) }
        return .terminateLater
    }
}

import AppKit
import Foundation

@MainActor
final class DaemonProcess {
    static let shared = DaemonProcess()
    private var child: Process?
    private var log: FileHandle?
    let root: String
    private init() {
        let args = CommandLine.arguments
        if let index = args.firstIndex(of: "--data-dir"), args.count > index + 1 { root = args[index + 1] }
        else { root = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Blakeswap").path }
    }
    func start() throws {
        if child?.isRunning == true { return }
        guard let resources = Bundle.main.resourceURL else { throw RPCError.message("App resources are missing.") }
        try FileManager.default.createDirectory(atPath: root, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let path = "\(root)/desktop.log"
        if !FileManager.default.fileExists(atPath: path) { FileManager.default.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        log = try FileHandle(forWritingTo: URL(fileURLWithPath: path)); try log?.seekToEnd()
        let process = Process()
        process.executableURL = resources.appendingPathComponent("blakeswap")
        process.arguments = ["desktop", "--data-dir", root, "--parent-pid", String(ProcessInfo.processInfo.processIdentifier)]
        process.standardOutput = log; process.standardError = log
        try process.run(); child = process
    }
    var failure: String? {
        guard let child, !child.isRunning else { return nil }
        return "Daemon exited (\(child.terminationStatus)). See \(root)/desktop.log."
    }
    func stop() async {
        guard let process = child else { return }
        if process.isRunning { process.terminate() }
        // Only wait on our owned child. It owns wallet state and never starts external chain nodes.
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

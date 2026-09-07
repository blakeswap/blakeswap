import AppKit
import Foundation
import SwiftUI
import Darwin

@MainActor
final class DaemonProcess {
    static let shared = DaemonProcess()
    private var child: Process?
    private var stopping = false
    private let executable: URL?
    private var log: FileHandle?
    private let shutdownTimeout: TimeInterval
 var isRunning: Bool { child?.isRunning == true }
 let root: String
    init(root: String? = nil, executable: URL? = nil, shutdownTimeout: TimeInterval = 8) {
 self.shutdownTimeout = shutdownTimeout
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
    func waitUntilReady(profile: String, timeout: TimeInterval = 15) async throws {
        let deadline = ProcessInfo.processInfo.systemUptime + timeout
        while true {
            try Task.checkCancellation()
            guard !stopping else { throw CancellationError() }
            guard let process = child else { throw RPCError.message("The wallet service has not been started.") }
            guard process.isRunning else {
                throw RPCError.message("The wallet service exited during startup (code \(process.terminationStatus)). Reopen Blakeswap or check desktop.log for details.")
            }
            do {
                _ = try DaemonRPC.endpoint(root: root, profile: profile)
                return
            } catch let error as CocoaError where error.code == .fileNoSuchFile || error.code == .fileReadNoSuchFile {
                // The helper publishes its private manifest only after opening its API listeners.
                guard ProcessInfo.processInfo.systemUptime < deadline else {
                    throw RPCError.message("The wallet service did not become ready. Try reopening Blakeswap.")
                }
                try await Task.sleep(nanoseconds: 50_000_000)
            }
        }
    }
    func stop() async {
        stopping = true
        guard let process = child else { return }
        let runtime = URL(fileURLWithPath: root).appendingPathComponent("runtime.json")
        let endpoints = (try? Data(contentsOf: runtime)).flatMap { try? JSONDecoder().decode([String: DaemonEndpoint].self, from: $0) } ?? [:]
        if process.isRunning { process.terminate() }
        let deadline = ProcessInfo.processInfo.systemUptime + shutdownTimeout
        while process.isRunning && ProcessInfo.processInfo.systemUptime < deadline {
            try? await Task.sleep(nanoseconds: 25_000_000)
        }
        if process.isRunning { kill(process.processIdentifier, SIGKILL) }
        await Task.detached { process.waitUntilExit() }.value
        // A forced exit cannot run Go defers. Remove only the owned helper's
        // validated private temporary runtime; external node services are untouched.
        for directory in Set(endpoints.values.map { URL(fileURLWithPath: $0.socket).deletingLastPathComponent() }) {
            let temporary = URL(fileURLWithPath: NSTemporaryDirectory()).resolvingSymlinksInPath()
            let resolved = directory.resolvingSymlinksInPath()
            if resolved.deletingLastPathComponent() == temporary && resolved.lastPathComponent.hasPrefix("blakeswap-"),
               let a = try? FileManager.default.attributesOfItem(atPath: directory.path),
               a[.type] as? FileAttributeType == .typeDirectory,
               let mode = a[.posixPermissions] as? NSNumber, mode.intValue & 0o077 == 0 {
                try? FileManager.default.removeItem(at: directory)
            }
        }
        try? FileManager.default.removeItem(at: runtime)
        try? log?.close(); log = nil; child = nil
    }
}


@MainActor
final class ShutdownCoordinator {
    private let daemon: DaemonProcess
    private let summary: () async throws -> ActionSummary
    private let decision: (ActionSummary?) async -> Bool
    private var deciding = false
    init(daemon: DaemonProcess, summary: @escaping () async throws -> ActionSummary, decision: @escaping (ActionSummary?) async -> Bool) {
        self.daemon = daemon; self.summary = summary; self.decision = decision
    }
    func requestQuit() async -> Bool {
        guard !deciding else { return false }; deciding = true; defer { deciding = false }
        let current = try? await summary()
        // Unknown is distinct from an actual obligation, and still offers an
        // explicit Quit. A successful empty/settled check needs no prompt.
        if current == nil || current!.requiresMonitoring || !current!.complete {
            guard await decision(current) else { return false }
        }
        await daemon.stop()
        return true
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var coordinator: ShutdownCoordinator?
    private var observers: [NSObjectProtocol] = []
    private var terminating = false
    func configure(model: AppModel) {
        guard coordinator == nil else { return }
        coordinator = ShutdownCoordinator(daemon: .shared, summary: { try await model.actionSummary() }, decision: Self.confirmQuit)
        let center = NSWorkspace.shared.notificationCenter
        observers.append(center.addObserver(forName: NSWorkspace.willSleepNotification, object: nil, queue: .main) { _ in Task { @MainActor in model.monitoring.interruption() } })
        observers.append(center.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { _ in Task { @MainActor in model.monitoring.interruption(); await model.refreshMonitoring(refresh: true) } })
        observers.append(center.addObserver(forName: NSWorkspace.sessionDidBecomeActiveNotification, object: nil, queue: .main) { _ in Task { @MainActor in await model.refreshMonitoring(refresh: true) } })
    }
    static func confirmQuit(_ summary: ActionSummary?) async -> Bool {
        let alert = NSAlert()
        alert.messageText = "Stop Blakeswap monitoring?"
        var lines = ["Quitting stops the wallet daemon and locally accepted rescue jobs. Notifications do not monitor chains while the app is closed."]
        if let summary {
            for wallet in summary.wallets {
                if !wallet.known { lines.append("\(wallet.walletID): local obligation state could not be checked.") }
                for action in wallet.actions where action.requiresMonitoring {
                    lines.append("\(wallet.walletID): \(action.monitoringTitle)." + (action.firstReveal ? " This app must perform the first secret revelation; an external tower cannot do it." : ""))
                }
            }
        } else { lines.append("The helper could not provide an all-wallet check. Outstanding obligations may still need monitoring.") }
        alert.informativeText = lines.joined(separator: "\n\n")
        alert.addButton(withTitle: "Stay open"); alert.addButton(withTitle: "Quit")
        return alert.runModal() == .alertSecondButtonReturn
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard !terminating else { return .terminateCancel }; terminating = true
        let coordinator = coordinator ?? ShutdownCoordinator(daemon: .shared, summary: { throw RPCError.message("Opening") }, decision: Self.confirmQuit)
        Task {
            let quit = await coordinator.requestQuit()
            terminating = false
            sender.reply(toApplicationShouldTerminate: quit)
        }
        return .terminateLater
    }
}

// Intercept the last close before SwiftUI destroys its window. Stay open then
// leaves both the window and helper alive. Forward other window behavior.
struct LastWindowGuard: NSViewRepresentable {
    final class GuardView: NSView, NSWindowDelegate {
        weak var previous: NSWindowDelegate?
 var terminate: () -> Void = { NSApp.terminate(nil) }
 var hasOtherWindows: ((NSWindow) -> Bool)?
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            if let window, window.delegate !== self { previous = window.delegate; window.delegate = self }
        }
        override func responds(to selector: Selector!) -> Bool { super.responds(to: selector) || (previous?.responds(to: selector) ?? false) }
        override func forwardingTarget(for selector: Selector!) -> Any? { previous?.responds(to: selector) == true ? previous : super.forwardingTarget(for: selector) }
        func windowShouldClose(_ sender: NSWindow) -> Bool {
            let others = hasOtherWindows?(sender) ?? NSApp.windows.contains { $0 !== sender && $0.isVisible && $0.canBecomeMain }
            if !others { terminate(); return false }
            return previous?.windowShouldClose?(sender) ?? true
        }
    }
    func makeNSView(context: Context) -> GuardView { GuardView() }
    func updateNSView(_ nsView: GuardView, context: Context) {}
}

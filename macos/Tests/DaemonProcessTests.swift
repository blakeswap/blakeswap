import AppKit
import XCTest
import Foundation
@testable import Blakeswap

final class DaemonProcessTests: XCTestCase {
    @MainActor
    func testExitedHelperRestartsAndShutdownPreventsRelaunch() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let script = root.appendingPathComponent("helper")
        // Record each launch, then exit to simulate a helper crash.
        try Data("#!/bin/sh\necho started >> \"$3/starts\"\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let process = DaemonProcess(root: root.path, executable: script)
        func launches() -> Int { ((try? String(contentsOf: root.appendingPathComponent("starts"), encoding: .utf8)) ?? "").split(separator: "\n").count }
        try process.start()
        for _ in 0..<100 {
            if launches() == 1 { break }
            try await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTAssertEqual(launches(), 1)
        for _ in 0..<100 {
            try process.start()
            if launches() == 2 { break }
            try await Task.sleep(nanoseconds: 10_000_000)
        }
        await process.stop()
        let stoppedCount = launches()
        XCTAssertGreaterThanOrEqual(stoppedCount, 2)
        try process.start()
        XCTAssertEqual(launches(), stoppedCount)
    }
}

extension DaemonProcessTests {
    @MainActor
    func testPendingInstallationOffersStayOpenAndExplicitQuit() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the freshly built helper") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let daemon = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper))
        addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: root) }
        try daemon.start(); try await daemon.waitUntilReady(profile: "alice")
        var value = ActionSummary(); value.complete = true; value.installationPending = true
        var quit = false; var prompts = 0
        let coordinator = ShutdownCoordinator(daemon: daemon, summary: { value }, decision: { summary in
            XCTAssertTrue(summary?.installationPending == true); prompts += 1; return quit
        })
        let stayed = await coordinator.requestQuit()
        XCTAssertFalse(stayed); XCTAssertTrue(daemon.isRunning)
        quit = true
        let stopped = await coordinator.requestQuit()
        XCTAssertTrue(stopped); XCTAssertFalse(daemon.isRunning); XCTAssertEqual(prompts, 2)
    }
    @MainActor
    func testSecondActualHelperCannotRemoveFirstOwnersRuntime() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the freshly built helper") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let first = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper))
        let second = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper))
        addTeardownBlock { await second.stop(); await first.stop(); try? FileManager.default.removeItem(at: root) }
        try first.start(); try await first.waitUntilReady(profile: "alice")
        let runtime = root.appendingPathComponent("runtime.json")
        let original = try Data(contentsOf: runtime)
        let endpoint = try DaemonRPC.endpoint(root: root.path, profile: "alice")
        try second.start()
        do { try await second.waitUntilReady(profile: "alice"); XCTFail("Another owner's manifest cannot make this child ready") } catch {}
        // The second child cannot acquire desktop.lock. Its exit does not grant
        // ownership of the runtime published by the first child.
        for _ in 0..<200 { if !second.isRunning { break }; try await Task.sleep(nanoseconds: 10_000_000) }
        XCTAssertFalse(second.isRunning)
        await second.stop()
        XCTAssertTrue(first.isRunning)
        XCTAssertEqual(try? Data(contentsOf: runtime), original)
        XCTAssertTrue(FileManager.default.fileExists(atPath: endpoint.socket))
        XCTAssertTrue(FileManager.default.fileExists(atPath: endpoint.socket + ".json"))
        let status = try await DaemonRPC.call(root: root.path, profile: "alice", method: "status")
        XCTAssertFalse(status.isEmpty)
    }
    @MainActor
    func testForcedActualHelperCleanupUsesItsLaunchIdentity() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the freshly built helper") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let daemon = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper), shutdownTimeout: 0.15)
        addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: root) }
        try daemon.start(); try await daemon.waitUntilReady(profile: "alice")
        let endpoint = try DaemonRPC.endpoint(root: root.path, profile: "alice")
        let pid = try XCTUnwrap(endpoint.ownerPID)
        XCTAssertFalse(try XCTUnwrap(endpoint.ownerSession).isEmpty)
        // Suspend only our isolated child so Go defers cannot handle SIGTERM.
        XCTAssertEqual(kill(pid, SIGSTOP), 0)
        await daemon.stop()
        XCTAssertFalse(daemon.isRunning)
        XCTAssertFalse(FileManager.default.fileExists(atPath: root.appendingPathComponent("runtime.json").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: endpoint.socket))
        XCTAssertFalse(FileManager.default.fileExists(atPath: endpoint.socket + ".json"))
    }
    @MainActor
    func testActualHelperLastWindowStayOpenThenExplicitQuitCleansRuntime() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the freshly built helper") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let daemon = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper))
        addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: root) }
        try daemon.start(); try await daemon.waitUntilReady(profile: "alice")
        let endpoint = try DaemonRPC.endpoint(root: root.path, profile: "alice")
        var summary = ActionSummary(); summary.complete = true; summary.requiresMonitoring = true
        var other = Blakeswap_V1_WalletActions(); other.walletID = "other-wallet"; other.known = true
        var job = WalletAction(); job.kind = "tower"; job.state = "tower_monitoring"; job.requiresMonitoring = true
        other.actions = [job]; summary.wallets = [other]
        var quit = false; var prompted = 0
        let coordinator = ShutdownCoordinator(daemon: daemon, summary: { summary }, decision: { current in
            prompted += 1; XCTAssertEqual(current?.wallets.first?.walletID, "other-wallet"); return quit
        })
        let guardView = LastWindowGuard.GuardView()
        guardView.hasOtherWindows = { _ in false }
        var attempt: Task<Bool, Never>?
        guardView.terminate = { attempt = Task { await coordinator.requestQuit() } }
        let window = NSWindow(contentRect: .zero, styleMask: [.titled, .closable], backing: .buffered, defer: true)
        XCTAssertFalse(guardView.windowShouldClose(window))
        let stayed = await attempt!.value
        XCTAssertFalse(stayed); XCTAssertEqual(prompted, 1); XCTAssertTrue(daemon.isRunning)
        XCTAssertNoThrow(try DaemonRPC.endpoint(root: root.path, profile: "alice"))
        quit = true
        XCTAssertFalse(guardView.windowShouldClose(window))
        let stopped = await attempt!.value
        XCTAssertTrue(stopped); XCTAssertEqual(prompted, 2); XCTAssertFalse(daemon.isRunning)
        XCTAssertFalse(FileManager.default.fileExists(atPath: root.appendingPathComponent("runtime.json").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: endpoint.socket))
        XCTAssertFalse(FileManager.default.fileExists(atPath: endpoint.socket + ".json"))
        try daemon.start(); XCTAssertFalse(daemon.isRunning)
    }
    @MainActor
    func testActualEmptyHelperQuitsWithoutPromptAndUnknownCanExplicitlyQuit() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the freshly built helper") }
        for unavailable in [false, true] {
            let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
            let daemon = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper))
            addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: root) }
            try daemon.start(); try await daemon.waitUntilReady(profile: "alice")
            var prompted = false
            let coordinator = ShutdownCoordinator(daemon: daemon, summary: {
                if unavailable { throw RPCError.message("Unavailable") }
                let raw = try await DaemonRPC.call(root: root.path, profile: "alice", method: "actions.summary")
                return try ActionSummary(serializedBytes: raw)
            }, decision: { current in prompted = true; XCTAssertNil(current); return true })
            let result = await coordinator.requestQuit()
            XCTAssertTrue(result); XCTAssertEqual(prompted, unavailable); XCTAssertFalse(daemon.isRunning)
            XCTAssertFalse(FileManager.default.fileExists(atPath: root.appendingPathComponent("runtime.json").path))
        }
    }
    @MainActor
    func testUnresponsiveOwnedChildHasBoundedShutdown() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let script = root.appendingPathComponent("ignores-term")
        try Data("#!/bin/sh\ntrap '' TERM\necho ready > \"$3/ready\"\nwhile :; do :; done\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let daemon = DaemonProcess(root: root.path, executable: script, shutdownTimeout: 0.15)
        try daemon.start()
        for _ in 0..<100 { if FileManager.default.fileExists(atPath: root.appendingPathComponent("ready").path) { break }; try await Task.sleep(nanoseconds: 10_000_000) }
        let started = ProcessInfo.processInfo.systemUptime
        await daemon.stop()
        XCTAssertLessThan(ProcessInfo.processInfo.systemUptime - started, 3)
        XCTAssertFalse(daemon.isRunning)
    }
}

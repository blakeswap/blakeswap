import Foundation
import XCTest
@testable import Blakeswap

final class AppStartupTests: XCTestCase {
    @MainActor
    func testDelayedHelperStartupDoesNotPublishMissingRuntimeError() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else {
            throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the built desktop helper")
        }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let script = directory.appendingPathComponent("delayed-helper")
        // Keep the helper alive, but hold back its endpoint until the test releases it.
        let quotedHelper = "'" + helper.replacingOccurrences(of: "'", with: "'\\''") + "'"
        try Data("""
        #!/bin/sh
        touch "$3/started"
        while [ ! -f "$3/release" ]; do sleep 0.01; done
        exec \(quotedHelper) "$@"
        """.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let daemon = DaemonProcess(root: directory.path, executable: script)
        addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: directory) }
        let model = AppModel(daemon: daemon)
        let refresh = Task { await model.refresh() }
        defer { refresh.cancel() }
        for _ in 0..<100 {
            if FileManager.default.fileExists(atPath: directory.appendingPathComponent("started").path) { break }
            try await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTAssertTrue(FileManager.default.fileExists(atPath: directory.appendingPathComponent("started").path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.appendingPathComponent("runtime.json").path))
        XCTAssertNil(model.connectionError, "Expected startup must stay on the loading screen without a file error")
        XCTAssertNil(model.settings)
        try Data().write(to: directory.appendingPathComponent("release"))
        await refresh.value
        XCTAssertNil(model.connectionError)
        XCTAssertEqual(model.settings?.onboardingStage, "wallet", "The same refresh must continue once the endpoint is ready")
        await daemon.stop()
    }

    @MainActor
    private func fixture(script contents: String = "exec /bin/sleep 30") throws -> (URL, DaemonProcess) {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let script = root.appendingPathComponent("helper")
        try Data(("#!/bin/sh\n" + contents + "\n").utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let daemon = DaemonProcess(root: root.path, executable: script)
        addTeardownBlock { await daemon.stop(); try? FileManager.default.removeItem(at: root) }
        return (root, daemon)
    }

    @MainActor
    func testMissingRuntimeTimesOutWithActionableError() async throws {
        let (_, daemon) = try fixture()
        try daemon.start()
        do {
            try await daemon.waitUntilReady(profile: "alice", timeout: 0)
            XCTFail("Missing runtime was treated as ready")
        } catch {
            XCTAssertEqual(error.localizedDescription, "The wallet service did not become ready. Try reopening Blakeswap.")
        }
    }

    @MainActor
    func testExitedHelperReportsItsExitInsteadOfMissingRuntime() async throws {
        let (_, daemon) = try fixture(script: "exit 7")
        try daemon.start()
        do {
            try await daemon.waitUntilReady(profile: "alice")
            XCTFail("Exited helper was treated as ready")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("exited during startup (code 7)"), error.localizedDescription)
        }
    }

    @MainActor
    func testUnsafeAndInvalidRuntimeFilesAreNotRetriedAsStartup() async throws {
        let (root, daemon) = try fixture()
        try daemon.start()
        let runtime = root.appendingPathComponent("runtime.json")
        try Data("{}".utf8).write(to: runtime)
        for (mode, message) in [(0o644, "Daemon runtime file must be private."), (0o600, "Invalid daemon endpoint.")] {
            try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: runtime.path)
            do {
                try await daemon.waitUntilReady(profile: "alice", timeout: 0)
                XCTFail("Unsafe runtime was accepted")
            } catch { XCTAssertEqual(error.localizedDescription, message) }
        }
    }

    @MainActor
    func testCancelledAndStoppedStartupWaitsDoNotContinue() async throws {
        let (_, daemon) = try fixture()
        try daemon.start()
        let waiting = Task { try await daemon.waitUntilReady(profile: "alice") }
        waiting.cancel()
        do { try await waiting.value; XCTFail("Cancelled startup continued") }
        catch is CancellationError {} catch { XCTFail("Unexpected cancellation error: \(error)") }
        await daemon.stop()
        do { try await daemon.waitUntilReady(profile: "alice"); XCTFail("Stopped startup continued") }
        catch is CancellationError {} catch { XCTFail("Unexpected shutdown error: \(error)") }
    }

    @MainActor
    func testLaunchFailureIsNotOverwrittenByRuntimeFileError() async throws {
        let (root, _) = try fixture()
        let daemon = DaemonProcess(root: root.path, executable: root.appendingPathComponent("missing-helper"))
        addTeardownBlock { await daemon.stop() }
        var expected: String?
        do { try daemon.start(); XCTFail("Missing helper started") }
        catch { expected = error.localizedDescription }
        let model = AppModel(daemon: daemon)
        await model.refresh()
        XCTAssertNotNil(expected)
        XCTAssertEqual(model.connectionError, expected)
    }
}

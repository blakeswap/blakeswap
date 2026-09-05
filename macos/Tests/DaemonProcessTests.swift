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

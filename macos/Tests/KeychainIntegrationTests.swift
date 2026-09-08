import Foundation
import XCTest
@testable import Blakeswap

final class KeychainIntegrationTests: XCTestCase {
    func testSignedAppOwnsUniqueItemAcrossProcessRestart() throws {
        guard let executable = ProcessInfo.processInfo.environment["BLAKESWAP_KEYCHAIN_TEST_APP"] else {
            throw XCTSkip("Set BLAKESWAP_KEYCHAIN_TEST_APP for the explicitly scoped signed-app Keychain integration")
        }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("blakeswap-keychain-test-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        let record = root.appendingPathComponent("item.json")
        func run(_ operation: String) throws -> Int32 {
            let process = Process(), output = Pipe()
            process.executableURL = URL(fileURLWithPath: executable)
            process.arguments = ["--keychain-integration", operation, record.path]
            process.standardOutput = output; process.standardError = output
            try process.run()
            let bytes = output.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            // The command emits a sanitized status only, never account bytes.
            let status = String(data: bytes, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            XCTAssertTrue(["ok", "locked", "denied", "cancelled", "missing", "exists", "conflict", "unavailable", "changed", "invalid", "closed"].contains(status), "Unexpected test command output")
            return process.terminationStatus
        }
        var removed = false
        defer {
            if !removed, FileManager.default.fileExists(atPath: record.path) {
                removed = (try? run("delete")) == 0
            }
            // Retain the public test record if OS denial prevents cleanup.
            if removed { try? FileManager.default.removeItem(at: root) }
            else if FileManager.default.fileExists(atPath:record.path) { XCTFail("Owned test item cleanup incomplete; public recovery record: " + record.path) }
        }
        XCTAssertEqual(try run("create"), 0, "Test-only Keychain creation or duplicate/identity controls failed")
        XCTAssertTrue(FileManager.default.fileExists(atPath: record.path))
        XCTAssertEqual(try run("read"), 0, "The same signed app could not read its item in a new process")
        let cleanup = try run("delete")
        removed = cleanup == 0
        XCTAssertEqual(cleanup, 0, "Owned test item cleanup failed")
    }
}

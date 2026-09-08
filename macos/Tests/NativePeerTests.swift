import Foundation
import XCTest
@testable import Blakeswap

final class NativePeerTests: XCTestCase {
    func testConcurrentRepliesCancellationAndContinuedUse() async throws {
        let inbound = Pipe(), outbound = Pipe()
        let began = expectation(description: "cancelled handler began")
        let a = NativePeer(input: inbound.fileHandleForReading, output: outbound.fileHandleForWriting, session: "isolated-session") { _, raw in raw }
        let b = NativePeer(input: outbound.fileHandleForReading, output: inbound.fileHandleForWriting, session: "isolated-session") { method, raw in
            if method == "wait" { began.fulfill(); try await Task.sleep(nanoseconds: 60_000_000_000) }
            return raw
        }
        defer { a.close(); b.close() }
        try await withThrowingTaskGroup(of: Void.self) { group in
            for index in 0..<16 {
                group.addTask {
                    let raw = Data(String(index).utf8)
                    let reply = try await a.call("echo", payload: raw)
                    XCTAssertEqual(reply, raw)
                }
            }
            try await group.waitForAll()
        }
        let pending = Task { try await a.call("wait", payload: Data("true".utf8)) }
        await fulfillment(of: [began], timeout: 2)
        pending.cancel()
        do { _ = try await pending.value; XCTFail("Cancelled request returned a reply") } catch {}
        let reply = try await a.call("echo", payload: Data("false".utf8))
        XCTAssertEqual(reply, Data("false".utf8))
        b.close()
        do { _ = try await a.call("echo", payload: Data("null".utf8)); XCTFail("Closed peer answered") } catch {}
    }
    func testWrongSessionFailsWithoutAcceptingReply() async throws {
        let inbound = Pipe(), outbound = Pipe()
        let a = NativePeer(input: inbound.fileHandleForReading, output: outbound.fileHandleForWriting, session: "first") { _, raw in raw }
        let b = NativePeer(input: outbound.fileHandleForReading, output: inbound.fileHandleForWriting, session: "other") { _, raw in raw }
        defer { a.close(); b.close() }
        do { _ = try await a.call("echo", payload: Data("true".utf8)); XCTFail("Different session was accepted") } catch {}
        XCTAssertTrue(a.isClosed)
    }
    func testOversizedFrameClosesBeforeReadingItsPayload() async throws {
        let inbound = Pipe(), outbound = Pipe()
        let peer = NativePeer(input: inbound.fileHandleForReading, output: outbound.fileHandleForWriting, session: "isolated") { _, raw in raw }
        defer { peer.close(); try? inbound.fileHandleForWriting.close(); try? outbound.fileHandleForReading.close() }
        var count = UInt32(131_073).bigEndian
        try withUnsafeBytes(of: &count) { try inbound.fileHandleForWriting.write(contentsOf: $0) }
        do { _ = try await peer.call("echo", payload: Data("true".utf8)); XCTFail("Oversized frame remained usable") } catch {}
        XCTAssertTrue(peer.isClosed)
    }
}

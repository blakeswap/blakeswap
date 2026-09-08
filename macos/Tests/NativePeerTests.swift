import Foundation
import XCTest
import Darwin
@testable import Blakeswap

final class NativePeerTests: XCTestCase {
    private func frame(_ kind: String, id: String, session: String) throws -> Data {
        let raw = try JSONSerialization.data(withJSONObject: ["version": 1, "session": session, "id": id, "kind": kind, "method": "echo", "payload": true])
        var size = UInt32(raw.count).bigEndian
        var packet = withUnsafeBytes(of: &size) { Data($0) }; packet.append(raw)
        return packet
    }
    private func waitFor(_ condition: () -> Bool) async throws {
        let deadline = Date().addingTimeInterval(2)
        while !condition() && Date() < deadline { try await Task.sleep(nanoseconds: 1_000_000) }
        XCTAssertTrue(condition(), "Private pipe did not reach the expected bounded state")
    }
    private func available(_ handle: FileHandle) -> Int32 {
        var descriptor = pollfd(fd: handle.fileDescriptor, events: Int16(POLLIN), revents: 0)
        return poll(&descriptor, 1, 0) > 0 && descriptor.revents & Int16(POLLIN) != 0 ? 1 : 0
    }
    func testCancellingPartiallyWrittenRequestClosesAndReleasesAllQueuedBytes() async throws {
        let input = Pipe(), output = Pipe()
        let peer = NativePeer(input: input.fileHandleForReading, output: output.fileHandleForWriting, session: "partial-request") { _, raw in raw }
        defer { peer.close(); try? input.fileHandleForWriting.close(); try? output.fileHandleForReading.close() }
        let raw = try JSONSerialization.data(withJSONObject: ["synthetic": String(repeating: "x", count: 120_000)])
        let call = Task { try await peer.call("echo", payload: raw) }
        try await waitFor { self.available(output.fileHandleForReading) > 0 && peer.queuedWrites.bytes > 0 }
        call.cancel()
        do { _ = try await call.value; XCTFail("Cancelled partial request returned") } catch {}
        XCTAssertTrue(peer.isClosed, "A partial frame cannot be replaced with a cancel frame")
        XCTAssertEqual(peer.queuedWrites.count, 0); XCTAssertEqual(peer.queuedWrites.bytes, 0)
    }
    func testCancellingUnwrittenRequestDiscardsItAndKeepsStreamUsable() async throws {
        let input = Pipe(), output = Pipe(), session = "unsent-request"
        let peer = NativePeer(input: input.fileHandleForReading, output: output.fileHandleForWriting, session: session) { _, raw in raw }
        var remote: NativePeer?
        defer { peer.close(); remote?.close(); try? input.fileHandleForWriting.close(); try? output.fileHandleForReading.close() }
        // Occupy the pipe before any protocol frame. No test reader is active.
        let filler = Data(repeating: 0, count: 4096)
        var occupied = 0
        while true {
            let n = filler.withUnsafeBytes { Darwin.write(output.fileHandleForWriting.fileDescriptor, $0.baseAddress!, $0.count) }
            if n <= 0 { XCTAssertEqual(errno, EAGAIN); break }; occupied += n
        }
        let call = Task { try await peer.call("echo", payload: Data("true".utf8)) }
        try await waitFor { peer.queuedWrites.count == 1 }
        call.cancel()
        do { _ = try await call.value; XCTFail("Cancelled unsent request returned") } catch {}
        XCTAssertFalse(peer.isClosed)
        XCTAssertEqual(peer.queuedWrites.bytes, 0)
        var remaining = occupied
        while remaining > 0 {
            let part = try XCTUnwrap(output.fileHandleForReading.read(upToCount: remaining))
            XCTAssertFalse(part.isEmpty); remaining -= part.count
        }
        remote = NativePeer(input: output.fileHandleForReading, output: input.fileHandleForWriting, session: session) { _, raw in raw }
        let reply = try await peer.call("echo", payload: Data("false".utf8))
        XCTAssertEqual(reply, Data("false".utf8), "Retired unsent frame corrupted the next exchange")
    }
    func testCancelledPartialCredentialReplyClosesAndClearsPayload() async throws {
        let input = Pipe(), output = Pipe(), session = "partial-response", id = String(repeating: "a", count: 64)
        let raw = try JSONSerialization.data(withJSONObject: ["synthetic": String(repeating: "x", count: 120_000)])
        let peer = NativePeer(input: input.fileHandleForReading, output: output.fileHandleForWriting, session: session) { _, _ in raw }
        defer { peer.close(); try? input.fileHandleForWriting.close(); try? output.fileHandleForReading.close() }
        try input.fileHandleForWriting.write(contentsOf: frame("request", id: id, session: session))
        try await waitFor { self.available(output.fileHandleForReading) > 0 && peer.queuedWrites.bytes > 0 }
        try input.fileHandleForWriting.write(contentsOf: frame("cancel", id: id, session: session))
        try await waitFor { peer.isClosed }
        XCTAssertEqual(peer.queuedWrites.bytes, 0); XCTAssertEqual(peer.queuedWrites.count, 0)
    }
    func testStalledReaderCannotAccumulateMoreThanBoundedResponses() async throws {
        let input = Pipe(), output = Pipe(), session = "bounded-responses"
        let raw = try JSONSerialization.data(withJSONObject: ["synthetic": String(repeating: "x", count: 65_536)])
        let peer = NativePeer(input: input.fileHandleForReading, output: output.fileHandleForWriting, session: session) { _, _ in raw }
        defer { peer.close(); try? input.fileHandleForWriting.close(); try? output.fileHandleForReading.close() }
        _ = fcntl(input.fileHandleForWriting.fileDescriptor, F_SETNOSIGPIPE, 1)
        for index in 0..<40 {
            if peer.isClosed { break }
            do { try input.fileHandleForWriting.write(contentsOf: frame("request", id: String(format: "%064x", index), session: session)) } catch { break }
            let queue = peer.queuedWrites
            XCTAssertLessThanOrEqual(queue.count, NativePeer.maxQueuedWrites)
            XCTAssertLessThanOrEqual(queue.bytes, NativePeer.maxQueuedBytes)
            // Allow each handler to finish so this tests queued output rather
            // than relying on the separate incoming-handler bound.
            try await Task.sleep(nanoseconds: 1_000_000)
        }
        try await waitFor { peer.isClosed }
        XCTAssertEqual(peer.queuedWrites.count, 0); XCTAssertEqual(peer.queuedWrites.bytes, 0)
    }
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

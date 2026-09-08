import Foundation
import Security
import Darwin

// Only an app-owned anonymous pipe pair is accepted. This is not a socket and
// exposes no network approval endpoint. Framing and queues are bounded on both
// sides; helper logs use stderr, never this channel.
final class NativePeer: @unchecked Sendable {
    typealias Handler = @Sendable (String, Data) async throws -> Data
    private let input: FileHandle
    private let output: FileHandle
    let session: String
    private let lock = NSLock()
    private let writes = DispatchQueue(label: "org.blakeswap.private-write")
    private let reads = DispatchQueue(label: "org.blakeswap.private-read")
    private var pending: [String: CheckedContinuation<Data, Error>] = [:]
    private var incoming: [String: Task<Void, Never>] = [:]
    private struct Write {
        var bytes: Data
        var offset = 0
        let id: String
        let kind: String
        let deadline = ProcessInfo.processInfo.systemUptime + 45
    }
    private var outgoing: [Write] = []
    private var outgoingBytes = 0
    private var writing = false
    private var startedRequests: Set<String> = []
    static let maxQueuedWrites = 32
    static let maxQueuedBytes = maxQueuedWrites * (131_072 + 4)
    var queuedWrites: (count: Int, bytes: Int) { lock.lock(); defer { lock.unlock() }; return (outgoing.count, outgoingBytes) }
    private var closed = false
    var isClosed: Bool { lock.lock(); defer { lock.unlock() }; return closed }
    private let handler: Handler
    private let onClose: @Sendable (NativePeer) -> Void
    private struct Closure {
        let replies: [String: CheckedContinuation<Data, Error>]
        let tasks: [String: Task<Void, Never>]
    }
    init(input: FileHandle, output: FileHandle, session: String, onClose: @escaping @Sendable (NativePeer) -> Void = { _ in }, handler: @escaping Handler) {
        self.input = input; self.output = output; self.session = session; self.handler = handler
        self.onClose = onClose
        let descriptor = output.fileDescriptor
        let flags = fcntl(descriptor, F_GETFL)
        guard flags >= 0, fcntl(descriptor, F_SETFL, flags | O_NONBLOCK) == 0,
              fcntl(descriptor, F_SETNOSIGPIPE, 1) == 0 else { close(); return }
        reads.async { [weak self] in self?.readLoop() }
    }
    func close() {
        lock.lock()
        let cleanup = closeLocked()
        lock.unlock()
        finishClose(cleanup)
    }
    private func closeLocked() -> Closure? {
        guard !closed else { return nil }
        closed = true
        let replies = pending; pending.removeAll()
        let tasks = incoming; incoming.removeAll()
        startedRequests.removeAll()
        for index in outgoing.indices { outgoing[index].bytes.resetBytes(in: 0..<outgoing[index].bytes.count) }
        outgoing.removeAll(); outgoingBytes = 0
        // Writes are nonblocking and hold this same lock only for write(2), so
        // closure joins any active syscall before releasing its descriptor or
        // owned payload. No blocked DispatchQueue closure retains a frame.
        try? output.close()
        return Closure(replies: replies, tasks: tasks)
    }
    private func finishClose(_ cleanup: Closure?) {
        guard let cleanup else { return }
        onClose(self)
        try? input.close()
        for task in cleanup.tasks.values { task.cancel() }
        for reply in cleanup.replies.values { reply.resume(throwing: NativeSecurityError.closed) }
    }
    func call(_ method: String, payload: Data) async throws -> Data {
        guard !method.isEmpty, method.utf8.count <= 64, payload.count < 131_072 else { throw NativeSecurityError.invalid }
        let id = try Self.randomID()
        return try await withTaskCancellationHandler(operation: {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                lock.lock()
                guard !closed, pending.count < 32, !Task.isCancelled else { lock.unlock(); continuation.resume(throwing: NativeSecurityError.closed); return }
                pending[id] = continuation
                lock.unlock()
                do {
                    let object = try JSONSerialization.jsonObject(with: payload, options: [.fragmentsAllowed])
                    try send(["version": 1, "session": session, "id": id, "kind": "request", "method": method, "payload": object])
                } catch { fail(id, error: NativeSecurityError.invalid) }
                DispatchQueue.global().asyncAfter(deadline: .now() + 45) { [weak self] in self?.fail(id, error: NativeSecurityError.changed) }
            }
        }, onCancel: { [weak self] in self?.fail(id, error: NativeSecurityError.cancelled) })
    }
    private func fail(_ id: String, error: Error) {
        lock.lock()
        let reply = pending.removeValue(forKey: id)
        let started = startedRequests.remove(id) != nil
        let partial = retireWrite(id, kind: "request")
        let cleanup = partial ? closeLocked() : nil
        lock.unlock()
        finishClose(cleanup)
        if let reply {
            // Removing an unsent frame keeps this stream usable. A partially
            // written frame cannot be followed by a cancel frame safely.
            reply.resume(throwing: error)
            if started && !partial { try? send(["version": 1, "session": session, "id": id, "kind": "cancel"]) }
        }
    }
    // Caller holds lock. Return true when the frame already entered the pipe.
    private func retireWrite(_ id: String, kind: String) -> Bool {
        guard let index = outgoing.firstIndex(where: { $0.id == id && $0.kind == kind }) else { return false }
        let partial = outgoing[index].offset > 0
        outgoingBytes -= outgoing[index].bytes.count
        outgoing[index].bytes.resetBytes(in: 0..<outgoing[index].bytes.count)
        outgoing.remove(at: index)
        return partial
    }
    private func send(_ frame: [String: Any]) throws {
        var bytes = try JSONSerialization.data(withJSONObject: frame, options: [.sortedKeys])
        defer { bytes.resetBytes(in: 0..<bytes.count) }
        guard !bytes.isEmpty, bytes.count <= 131_072 else { throw NativeSecurityError.invalid }
        guard let id = frame["id"] as? String, let kind = frame["kind"] as? String else { throw NativeSecurityError.invalid }
        var size = UInt32(bytes.count).bigEndian
        var packet = withUnsafeBytes(of: &size) { Data($0) }
        packet.append(bytes)
        lock.lock()
        guard !closed else { lock.unlock(); packet.resetBytes(in: 0..<packet.count); throw NativeSecurityError.closed }
        if (kind == "request" && pending[id] == nil) || (kind == "response" && incoming[id]?.isCancelled == true) {
            lock.unlock(); packet.resetBytes(in: 0..<packet.count); return
        }
        guard outgoing.count < Self.maxQueuedWrites, packet.count <= Self.maxQueuedBytes - outgoingBytes else {
            let cleanup = closeLocked()
            lock.unlock(); packet.resetBytes(in: 0..<packet.count); finishClose(cleanup); throw NativeSecurityError.closed
        }
        outgoing.append(Write(bytes: packet, id: id, kind: kind)); outgoingBytes += packet.count
        let begin = !writing; writing = true
        lock.unlock()
        if begin { writes.async { [weak self] in self?.writeAvailable() } }
    }
    private func writeAvailable() {
        while true {
            lock.lock()
            guard !closed, !outgoing.isEmpty else { writing = false; lock.unlock(); return }
            guard ProcessInfo.processInfo.systemUptime < outgoing[0].deadline else { lock.unlock(); close(); return }
            let offset = outgoing[0].offset, descriptor = output.fileDescriptor
            let written = outgoing[0].bytes.withUnsafeBytes { raw in
                Darwin.write(descriptor, raw.baseAddress!.advanced(by: offset), raw.count - offset)
            }
            let failure = errno
            if written > 0 {
                outgoing[0].offset += written
                if outgoing[0].kind == "request", pending[outgoing[0].id] != nil { startedRequests.insert(outgoing[0].id) }
                if outgoing[0].offset == outgoing[0].bytes.count {
                    _ = retireWrite(outgoing[0].id, kind: outgoing[0].kind)
                }
                lock.unlock()
            } else if written < 0 && failure == EINTR { lock.unlock() }
            else if written < 0 && (failure == EAGAIN || failure == EWOULDBLOCK) {
                lock.unlock()
                // Exactly one pump is scheduled, capturing no frame bytes.
                writes.asyncAfter(deadline: .now() + .milliseconds(10)) { [weak self] in self?.writeAvailable() }
                return
            } else { lock.unlock(); close(); return }
        }
    }
    private func readExactly(_ size: Int) throws -> Data {
        var result = Data(); result.reserveCapacity(size)
        while result.count < size {
            guard let part = try input.read(upToCount: size - result.count), !part.isEmpty else { throw NativeSecurityError.closed }
            result.append(part)
        }
        return result
    }
    private func readLoop() {
        defer { close() }
        do {
            while true {
                let prefix = try readExactly(4)
                let size = prefix.reduce(UInt32(0)) { ($0 << 8) | UInt32($1) }
                guard size > 0, size <= 131_072 else { throw NativeSecurityError.invalid }
                var raw = try readExactly(Int(size))
                defer { raw.resetBytes(in: 0..<raw.count) }
                guard let frame = try JSONSerialization.jsonObject(with: raw) as? [String: Any],
                      frame["version"] as? Int == 1, frame["session"] as? String == session,
                      let id = frame["id"] as? String, id.count == 64, id.allSatisfy(\.isHexDigit),
                      let kind = frame["kind"] as? String else { throw NativeSecurityError.invalid }
                let payload = try JSONSerialization.data(withJSONObject: frame["payload"] ?? NSNull(), options: [.fragmentsAllowed, .sortedKeys])
                switch kind {
                case "response":
                    let code = frame["error"] as? String ?? ""
                    guard code.count <= 48, code.allSatisfy({ $0.isASCII && ($0.isLowercase || $0 == "_") }) else { throw NativeSecurityError.invalid }
                    lock.lock(); let reply = pending.removeValue(forKey: id); startedRequests.remove(id); lock.unlock()
                    if code.isEmpty { reply?.resume(returning: payload) }
                    else { reply?.resume(throwing: NativeSecurityError(rawValue: code) ?? .unavailable) }
                case "cancel":
                    lock.lock()
                    incoming[id]?.cancel()
                    let partial = retireWrite(id, kind: "response")
                    let cleanup = partial ? closeLocked() : nil
                    lock.unlock()
                    finishClose(cleanup)
                case "request":
                    guard let method = frame["method"] as? String, !method.isEmpty, method.utf8.count <= 64 else { throw NativeSecurityError.invalid }
                    lock.lock()
                    guard incoming[id] == nil, incoming.count < 32 else { lock.unlock(); throw NativeSecurityError.invalid }
                    let task = Task { [weak self] in
                        guard let self else { return }
                        var response: [String: Any] = ["version": 1, "session": self.session, "id": id, "kind": "response"]
                        do {
                            var output = try await self.handler(method, payload)
                            defer { output.resetBytes(in: 0..<output.count) }
                            try Task.checkCancellation()
                            response["payload"] = try JSONSerialization.jsonObject(with: output, options: [.fragmentsAllowed])
                        } catch {
                            response["error"] = Task.isCancelled ? "cancelled" : (error as? NativeSecurityError)?.rawValue ?? "unavailable"
                        }
                        try? self.send(response)
                        self.finishedIncoming(id)
                    }
                    incoming[id] = task
                    lock.unlock()
                default: throw NativeSecurityError.invalid
                }
            }
        } catch { /* Never log framed payloads or OS credential details. */ }
    }
    private func finishedIncoming(_ id: String) { lock.lock(); defer { lock.unlock() }; incoming.removeValue(forKey: id) }
    static func randomID() throws -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else { throw NativeSecurityError.unavailable }
        return bytes.map { String(format: "%02x", $0) }.joined()
    }
}

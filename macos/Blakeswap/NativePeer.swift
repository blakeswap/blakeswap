import Foundation
import Security

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
    private var closed = false
    var isClosed: Bool { lock.lock(); defer { lock.unlock() }; return closed }
    private let handler: Handler
    init(input: FileHandle, output: FileHandle, session: String, handler: @escaping Handler) {
        self.input = input; self.output = output; self.session = session; self.handler = handler
        reads.async { [weak self] in self?.readLoop() }
    }
    func close() {
        lock.lock()
        guard !closed else { lock.unlock(); return }
        closed = true
        let replies = pending; pending.removeAll()
        let tasks = incoming; incoming.removeAll()
        lock.unlock()
        try? input.close(); try? output.close()
        for task in tasks.values { task.cancel() }
        for reply in replies.values { reply.resume(throwing: NativeSecurityError.closed) }
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
        lock.lock(); let reply = pending.removeValue(forKey: id); lock.unlock()
        if let reply {
            reply.resume(throwing: error)
            try? send(["version": 1, "session": session, "id": id, "kind": "cancel"])
        }
    }
    private func send(_ frame: [String: Any]) throws {
        let bytes = try JSONSerialization.data(withJSONObject: frame, options: [.sortedKeys])
        guard !bytes.isEmpty, bytes.count <= 131_072 else { throw NativeSecurityError.invalid }
        lock.lock(); let isClosed = closed; lock.unlock()
        guard !isClosed else { throw NativeSecurityError.closed }
        writes.async { [weak self] in
            guard let self else { return }
            var size = UInt32(bytes.count).bigEndian
            do {
                try withUnsafeBytes(of: &size) { try self.output.write(contentsOf: $0) }
                try self.output.write(contentsOf: bytes)
            } catch { self.close() }
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
                    lock.lock(); let reply = pending.removeValue(forKey: id); lock.unlock()
                    if code.isEmpty { reply?.resume(returning: payload) }
                    else { reply?.resume(throwing: NativeSecurityError(rawValue: code) ?? .unavailable) }
                case "cancel":
                    lock.lock(); let task = incoming[id]; lock.unlock(); task?.cancel()
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

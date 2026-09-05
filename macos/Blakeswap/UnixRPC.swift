import Foundation
import Darwin

enum UnixRPC {
    static func call(socket path: String, method: String, params: Data = Data("{}".utf8)) async throws -> Data {
        let request = Data("{\"method\":\"\(method)\",\"params\":".utf8) + params + Data("}\n".utf8)
        return try await Task.detached(priority: .userInitiated) {
            try exchange(path: path, request: request)
        }.value
    }

    private static func exchange(path: String, request: Data) throws -> Data {
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw RPCError.message("Could not create local connection.") }
        defer { Darwin.close(fd) }
        var noSignal: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout<Int32>.size))
        var timeout = timeval(tv_sec: 45, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        let name = Array(path.utf8) + [0]
        guard name.count <= MemoryLayout.size(ofValue: address.sun_path) else { throw RPCError.message("Socket path is too long.") }
        withUnsafeMutableBytes(of: &address.sun_path) { target in
            target.copyBytes(from: name)
        }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0 else { throw RPCError.message("Daemon unavailable. Start the local network to connect.") }
        try request.withUnsafeBytes { buffer in
            var sent = 0
            while sent < buffer.count {
                let n = Darwin.write(fd, buffer.baseAddress!.advanced(by: sent), buffer.count - sent)
                guard n > 0 else { throw RPCError.message("Local request failed.") }
                sent += n
            }
        }
        var reply = Data()
        var buffer = [UInt8](repeating: 0, count: 8192)
        while reply.count < 2_000_000 {
            let count = Darwin.read(fd, &buffer, buffer.count)
            guard count > 0 else { throw RPCError.message("Daemon response timed out or disconnected.") }
            reply.append(contentsOf: buffer.prefix(count))
            if reply.contains(10) { return reply }
        }
        throw RPCError.message("Daemon response exceeds local limit.")
    }
}

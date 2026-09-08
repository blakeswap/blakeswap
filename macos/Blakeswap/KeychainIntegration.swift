import CryptoKit
import Darwin
import Foundation

// An explicit validation command, entered before constructing the app or any
// profile. It can address only this test service and internally minted accounts;
// neither a production service nor credential bytes can be supplied by argv.
enum KeychainIntegration {
    private static let service = "org.blakeswap.test.credential.v1"
    private struct Record: Codable {
        let key: CredentialKey
        let digest: String
    }
    static func runIfRequested(_ arguments: [String]) -> Int32? {
        guard arguments.dropFirst().first == "--keychain-integration" else { return nil }
        guard arguments.count == 4, ["create", "read", "delete"].contains(arguments[2]),
              arguments[3].hasPrefix("/") else { print("invalid"); return 2 }
        let url = URL(fileURLWithPath: arguments[3])
        do {
            let store = KeychainCredentialStore(service: service)
            if arguments[2] == "create" {
                let key = CredentialKey(installation: try NativePeer.randomID(), profile: "integration", record: try NativePeer.randomID())
                var password = Data(try NativePeer.randomID().utf8)
                defer { password.resetBytes(in: 0..<password.count) }
                let record = Record(key: key, digest: SHA256.hash(data: password).map { String(format: "%02x", $0) }.joined())
                // The public test record survives ambiguous item creation, so a
                // later cleanup still addresses only this test's own account.
                let raw = try JSONEncoder().encode(record)
                let fd = Darwin.open(url.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o600)
                guard fd >= 0 else { throw NativeSecurityError.invalid }
                let file = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
                try file.write(contentsOf: raw); try file.synchronize(); try file.close()
                try store.create(key, password: password)
                do { try store.create(key, password: Data(repeating: 0x3a, count: 64)); throw NativeSecurityError.conflict }
                catch NativeSecurityError.exists {}
                try verify(record, store: store)
                let other = CredentialKey(installation: key.installation, profile: key.profile, record: try NativePeer.randomID())
                do { var unexpected = try store.get(other); unexpected.resetBytes(in: 0..<unexpected.count); throw NativeSecurityError.conflict }
                catch NativeSecurityError.missing {}
            } else {
                let record = try readRecord(url)
                if arguments[2] == "read" { try verify(record, store: store) }
                else {
                    do { try store.delete(record.key) } catch NativeSecurityError.missing {}
                    do { var unexpected = try store.get(record.key); unexpected.resetBytes(in: 0..<unexpected.count); throw NativeSecurityError.conflict }
                    catch NativeSecurityError.missing {}
                }
            }
            print("ok"); return 0
        } catch {
            // No OS details, credential bytes, or recovery material in output.
            print((error as? NativeSecurityError)?.rawValue ?? "unavailable"); return 1
        }
    }
    private static func readRecord(_ url: URL) throws -> Record {
        let fd = Darwin.open(url.path, O_RDONLY | O_NOFOLLOW)
        guard fd >= 0 else { throw NativeSecurityError.invalid }
        let file = FileHandle(fileDescriptor: fd, closeOnDealloc: true); defer { try? file.close() }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_mode & S_IFMT == S_IFREG, info.st_mode & 0o077 == 0, info.st_size <= 4096 else { throw NativeSecurityError.invalid }
        let bytes = try file.read(upToCount: 4097) ?? Data()
        guard bytes.count <= 4096 else { throw NativeSecurityError.invalid }
        let record = try JSONDecoder().decode(Record.self, from: bytes)
        guard record.key.valid, record.key.profile == "integration", record.digest.count == 64, record.digest.allSatisfy(\.isHexDigit) else { throw NativeSecurityError.invalid }
        return record
    }
    private static func verify(_ record: Record, store: CredentialStore) throws {
        var actual = try store.get(record.key)
        defer { actual.resetBytes(in: 0..<actual.count) }
        guard SHA256.hash(data: actual).map({ String(format: "%02x", $0) }).joined() == record.digest else { throw NativeSecurityError.conflict }
    }
}

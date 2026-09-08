import Foundation
import Security
import LocalAuthentication

struct CredentialKey: Codable, Hashable, Sendable {
    let installation: String
    let profile: String
    let record: String
    var account: String { installation + ":" + profile + ":" + record }
    var valid: Bool {
        installation.count == 64 && installation.allSatisfy(\.isHexDigit) &&
        record.count == 64 && record.allSatisfy(\.isHexDigit) &&
        !profile.isEmpty && profile.utf8.count <= 64 && profile.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-") }
    }
}

enum NativeSecurityError: String, Error, LocalizedError, Sendable {
    case locked, denied, cancelled, missing, exists, conflict, unavailable, changed, invalid, closed
    var errorDescription: String? {
        switch self {
        case .locked: return "The login Keychain is locked or access needs approval. Unlock it in macOS, then retry. Blakeswap will not use a password file."
        case .denied: return "Authentication or Keychain access was denied. The requested action was not authorized."
        case .cancelled: return "Authentication was cancelled. The requested action was not authorized."
        case .missing: return "This wallet's Keychain item is missing. Restore using its recovery phrase or portable backup."
        case .exists, .conflict: return "The existing Keychain item does not match this wallet installation."
        case .unavailable: return "Native authentication is unavailable. Set up a macOS login password and retry; Touch ID is optional."
        case .changed: return "The wallet, network, reviewed action, or helper session changed. Review the action again."
        case .invalid: return "The native security request is invalid."
        case .closed: return "The native security connection closed. Existing trades can continue. Reopen the app to establish a new session; quitting still checks all wallets."
        }
    }
}

protocol CredentialStore: Sendable {
    func get(_ key: CredentialKey) throws -> Data
    func create(_ key: CredentialKey, password: Data) throws
    func delete(_ key: CredentialKey) throws
}

// The native executable owns the Keychain ACL. The helper gets separately owned
// bytes only over its inherited pipes; it never queries the Keychain itself.
// All operations refuse OS interaction, so a helper vault read cannot open a
// prompt while holding its lifecycle lock. Initial/action LA prompts are separate.
final class KeychainCredentialStore: CredentialStore, @unchecked Sendable {
    private let service: String
    init(service: String = "org.blakeswap.wallet-credential.v1") { self.service = service }
    private func query(_ key: CredentialKey) throws -> [String: Any] {
        guard key.valid else { throw NativeSecurityError.invalid }
        let context = LAContext()
        context.interactionNotAllowed = true
        return [kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
                kSecAttrAccount as String: key.account,
                kSecAttrSynchronizable as String: false,
                kSecUseAuthenticationContext as String: context]
    }
    private func check(_ result: OSStatus) throws {
        switch result {
        case errSecSuccess: return
        case errSecItemNotFound: throw NativeSecurityError.missing
        case errSecDuplicateItem: throw NativeSecurityError.exists
        case errSecInteractionNotAllowed: throw NativeSecurityError.locked
        case errSecAuthFailed: throw NativeSecurityError.denied
        case errSecUserCanceled: throw NativeSecurityError.cancelled
        default: throw NativeSecurityError.unavailable
        }
    }
    func get(_ key: CredentialKey) throws -> Data {
        var q = try query(key)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        try check(SecItemCopyMatching(q as CFDictionary, &result))
        guard let bytes = result as? Data, (16...4096).contains(bytes.count) else { throw NativeSecurityError.invalid }
        return Data(bytes)
    }
    func create(_ key: CredentialKey, password: Data) throws {
        guard (16...4096).contains(password.count) else { throw NativeSecurityError.invalid }
        var q = try query(key)
        var application: SecTrustedApplication?
        try check(SecTrustedApplicationCreateFromPath(nil, &application))
        guard let application else { throw NativeSecurityError.unavailable }
        var access: SecAccess?
        try check(SecAccessCreate("Blakeswap wallet credential" as CFString, [application] as CFArray, &access))
        guard let access else { throw NativeSecurityError.unavailable }
        q[kSecAttrAccess as String] = access
        q[kSecAttrLabel as String] = "Blakeswap wallet credential"
        q[kSecValueData as String] = password
        try check(SecItemAdd(q as CFDictionary, nil))
        // Never update an existing item. The migration journal reconciles a
        // lost acknowledgement by reading and comparing the exact account.
        var actual = try get(key)
        defer { actual.resetBytes(in: 0..<actual.count) }
        guard actual == password else { throw NativeSecurityError.conflict }
    }
    func delete(_ key: CredentialKey) throws { try check(SecItemDelete(try query(key) as CFDictionary)) }
}

@MainActor
protocol OwnerAuthenticator: AnyObject {
    func authenticate(reason: String) async throws
    func cancel()
}
@MainActor
final class SystemOwnerAuthenticator: OwnerAuthenticator {
    private var context: LAContext?
    func authenticate(reason: String) async throws {
        guard context == nil else { throw NativeSecurityError.changed }
        let attempt = LAContext()
        attempt.localizedCancelTitle = "Cancel action"
        attempt.localizedFallbackTitle = "Use Mac password"
        var failure: NSError?
        // deviceOwnerAuthentication permits the OS-managed login-password
        // fallback; no app field ever asks for or stores the Mac password.
        guard attempt.canEvaluatePolicy(.deviceOwnerAuthentication, error: &failure) else { throw NativeSecurityError.unavailable }
        context = attempt
        defer { if context === attempt { context = nil }; attempt.invalidate() }
        do {
            let allowed = try await withTaskCancellationHandler(operation: {
                try await attempt.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)
            }, onCancel: { attempt.invalidate() })
            try Task.checkCancellation()
            guard allowed, context === attempt else { throw NativeSecurityError.changed }
        } catch let error as LAError {
            switch error.code {
            case .userCancel, .appCancel, .systemCancel: throw NativeSecurityError.cancelled
            case .authenticationFailed: throw NativeSecurityError.denied
            default: throw NativeSecurityError.unavailable
            }
        }
    }
    func cancel() { context?.invalidate(); context = nil }
}

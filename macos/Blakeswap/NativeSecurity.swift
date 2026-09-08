import Foundation
import Security

struct ConsentAction: Codable, Equatable, Sendable {
    let installation: String
    let wallet: String
    let walletKey: String
    let network: String
    let epoch: String
    let method: String
    let digest: String
    enum CodingKeys: String, CodingKey { case installation, wallet, network, epoch, method, digest; case walletKey = "wallet_key" }
}
struct ConsentChallenge: Codable, Equatable, Sendable {
    let id: String
    let session: String
    let action: ConsentAction
    let expires: Int64
}
private struct NativeCredentialRequest: Codable { let key: CredentialKey; var password: Data? }
private struct NativeCredentialReply: Codable { let password: Data }

@MainActor
final class NativeSecurity {
    private let store: CredentialStore
    private let authenticator: OwnerAuthenticator
    private let initiallyAuthorized: Bool
    private(set) var initialAuthorized: Bool
    private(set) var initialAttempted = false
    private(set) var generation: UInt64 = 0
    private var peer: NativePeer?
    private var root: String?
    private var session: String?
    private var ownerPID: Int32?
    private var installation: String?
    private(set) var credentialFailure: NativeSecurityError?
    init(store: CredentialStore = KeychainCredentialStore(), authenticator: OwnerAuthenticator? = nil, initiallyAuthorized: Bool = false) {
        self.store = store; self.authenticator = authenticator ?? SystemOwnerAuthenticator()
        self.initiallyAuthorized = initiallyAuthorized; self.initialAuthorized = initiallyAuthorized
    }
    static func sensitive(_ method: String) -> Bool {
        ["wallet.send", "transaction.bump", "offer.create", "swap.take", "trade.confirm", "offer.cancel", "automation.save", "automation.disable", "strategy.save", "strategy.stop", "wallet.recovery", "wallet.backup", "pause", "settings.update", "wallet.create", "backup.export", "backup.import", "onboarding.prepare", "onboarding.get", "onboarding.confirm", "onboarding.export", "onboarding.finish"].contains(method)
    }
    func unlock(retry: Bool = false) async throws {
        if initialAuthorized { return }
        guard !initialAttempted || retry else { throw NativeSecurityError.denied }
        initialAttempted = true
        let expected = generation
        try await authenticator.authenticate(reason: "Unlock Blakeswap wallets. Already authorized trades continue while the app stays open, including when the screen locks.")
        try Task.checkCancellation()
        guard generation == expected else { throw NativeSecurityError.changed }
        initialAuthorized = true
    }
    func attach(root: String, session: String, pid: Int32, input: FileHandle, output: FileHandle) throws {
        guard initialAuthorized else { throw NativeSecurityError.denied }
        closeConnection(requiresUnlock: false)
        self.root = root; self.session = session; ownerPID = pid
        let store = self.store
        peer = NativePeer(input: input, output: output, session: session) { [weak self] method, raw in
            guard let self else { throw NativeSecurityError.closed }
            do {
            let request = try JSONDecoder().decode(NativeCredentialRequest.self, from: raw)
            try await self.validateCredentialRequest(request.key, session: session)
            try Task.checkCancellation()
            switch method {
            case "credential.get":
                var value = try store.get(request.key)
                defer { value.resetBytes(in: 0..<value.count) }
                await self.recordCredentialFailure(nil,session:session)
                return try JSONEncoder().encode(NativeCredentialReply(password: value))
            case "credential.create":
                guard var password = request.password else { throw NativeSecurityError.invalid }
                defer { password.resetBytes(in: 0..<password.count) }
                try store.create(request.key, password: password)
                await self.recordCredentialFailure(nil,session:session)
                return Data("true".utf8)
            case "credential.delete":
                try store.delete(request.key)
                await self.recordCredentialFailure(nil,session:session)
                return Data("true".utf8)
            default: throw NativeSecurityError.invalid
            }
            } catch {
                await self.recordCredentialFailure((error as? NativeSecurityError) ?? .unavailable,session:session)
                throw error
            }
        }
        NativeSecurityRegistry.register(self, root: root, session: session)
    }
    private func recordCredentialFailure(_ error: NativeSecurityError?, session: String) {
        guard self.session == session else { return }
        credentialFailure = error
    }
    private func validateCredentialRequest(_ key: CredentialKey, session: String) throws {
        guard initialAuthorized, self.session == session, key.valid, peer != nil else { throw NativeSecurityError.changed }
        // The installation identity is public and read from the exact owned
        // helper's root. Do not let any private request select another Mac app
        // installation's credential account.
        guard let root else { throw NativeSecurityError.closed }
        let path = URL(fileURLWithPath: root).appendingPathComponent("installation.json")
        let attributes = try FileManager.default.attributesOfItem(atPath: path.path)
        guard attributes[.type] as? FileAttributeType == .typeRegular,
              let mode = attributes[.posixPermissions] as? NSNumber, mode.intValue & 0o077 == 0,
              (attributes[.size] as? NSNumber)?.intValue == 64,
              let current = String(data: try Data(contentsOf: path), encoding: .utf8), current == key.installation,
              installation == nil || installation == current else { throw NativeSecurityError.changed }
        installation = current
    }
    func authorize(endpoint: DaemonEndpoint, profile: String, method: String, payload: Data) async throws -> String? {
        guard Self.sensitive(method) else { return nil }
        guard let peer, let session, endpoint.ownerSession == session, endpoint.ownerPID == ownerPID else { throw NativeSecurityError.changed }
        guard !peer.isClosed else { throw NativeSecurityError.closed }
        let expected = generation
        let params = try JSONSerialization.jsonObject(with: payload, options: [.fragmentsAllowed])
        var preparation = try JSONSerialization.data(withJSONObject: ["profile": profile, "method": method, "params": params], options: [.sortedKeys])
        defer { preparation.resetBytes(in: 0..<preparation.count) }
        let data = try await peer.call("consent.prepare", payload: preparation)
        let challenge = try JSONDecoder().decode(ConsentChallenge.self, from: data)
        guard challenge.session == session, challenge.action.wallet == profile, challenge.action.method == method,
              challenge.expires > Int64(Date().timeIntervalSince1970), challenge.id.count == 64,
              challenge.id.allSatisfy(\.isHexDigit), generation == expected else { throw NativeSecurityError.changed }
        do {
            try await authenticator.authenticate(reason: Self.reason(method, wallet: profile, network: challenge.action.network))
            try Task.checkCancellation()
            guard generation == expected, self.session == session, self.peer === peer,
                  challenge.expires > Int64(Date().timeIntervalSince1970) else { throw NativeSecurityError.changed }
            // The prompt approves this exact immutable challenge, never a new
            // payload assembled from mutable fields after authentication.
            _ = try await peer.call("consent.approve", payload: JSONEncoder().encode(challenge))
            try Task.checkCancellation()
            guard generation == expected, self.session == session else { throw NativeSecurityError.changed }
            return challenge.id
        } catch {
            let cancellation = try? JSONSerialization.data(withJSONObject: ["id": challenge.id])
            if let cancellation { _ = try? await peer.call("consent.cancel", payload: cancellation) }
            throw error
        }
    }
    private static func reason(_ method: String, wallet: String, network: String) -> String {
        let operation: String
        if method.contains("recovery") || method.contains("export") || method == "wallet.backup" || method == "onboarding.get" { operation = "Reveal or export recovery material" }
        else if method.hasPrefix("automation.") || method.hasPrefix("strategy.") || method.hasPrefix("settings.") || method == "onboarding.finish" { operation = "Apply the reviewed spending or connection policy" }
        else if method.hasPrefix("onboarding.") || method == "wallet.create" || method == "backup.import" { operation = "Set up the reviewed wallet" }
        else { operation = "Authorize the exact reviewed wallet action" }
        return "\(operation) for \(wallet) on \(network)."
    }
    @discardableResult
    func revoke() -> Task<Void, Never> {
        generation &+= 1; authenticator.cancel()
        let peer = self.peer
        return Task { if let peer { _ = try? await peer.call("consent.revoke", payload: Data("{}".utf8)) } }
    }
    func closeConnection(requiresUnlock: Bool = true) {
        generation &+= 1; authenticator.cancel()
        if let root, let session { NativeSecurityRegistry.remove(root: root, session: session) }
        peer?.close(); peer = nil; root = nil; session = nil; ownerPID = nil; installation = nil
        credentialFailure = nil
        if requiresUnlock { initialAuthorized = initiallyAuthorized; initialAttempted = false }
    }
}

@MainActor
enum NativeSecurityRegistry {
    @MainActor final class Permission {
        let id: String
        private weak var security: NativeSecurity?
        private let generation: UInt64
        init(id: String, security: NativeSecurity, generation: UInt64) { self.id = id; self.security = security; self.generation = generation }
        func validateReply() throws {
            guard let security, security.generation == generation else { throw NativeSecurityError.changed }
        }
    }
    private final class Entry { weak var security: NativeSecurity?; init(_ security: NativeSecurity) { self.security = security } }
    private static var entries: [String: Entry] = [:]
    private static func key(root: String, session: String) -> String { root + "|" + session }
    static func register(_ security: NativeSecurity, root: String, session: String) { entries[key(root: root, session: session)] = Entry(security) }
    static func remove(root: String, session: String) { entries.removeValue(forKey: key(root: root, session: session)) }
    static func authorize(root: String, endpoint: DaemonEndpoint, profile: String, method: String, payload: Data) async throws -> Permission? {
        guard NativeSecurity.sensitive(method) else { return nil }
        // A deliberate headless file-mode helper has no native consent owner.
        if endpoint.credentialMode == "file" { return nil }
        guard let session = endpoint.ownerSession, let security = entries[key(root: root, session: session)]?.security else { throw NativeSecurityError.closed }
        let generation = security.generation
        guard let id = try await security.authorize(endpoint: endpoint, profile: profile, method: method, payload: payload) else { return nil }
        let permission = Permission(id:id,security:security,generation:generation)
        try permission.validateReply()
        return permission
    }
}

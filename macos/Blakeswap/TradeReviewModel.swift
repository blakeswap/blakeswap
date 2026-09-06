import Foundation
import CryptoKit
import SwiftProtobuf

struct TradeContext: Identifiable, Equatable {
    let id = UUID()
    let profile: String
    let network: String
    let generation: UInt64
    let walletKey: String
    func matches(_ other: TradeContext) -> Bool {
        profile == other.profile && network == other.network && generation == other.generation && walletKey == other.walletKey
    }
}

// Persist only the identity needed to retry an already-confirmed action. Amounts,
// signed offers, provider proofs and secrets stay out of the native journal.
struct PendingTradeConfirmation: Codable, Equatable {
    let profile: String
    let network: String
    let requestID: String
    let token: String
    let revision: String
    let kind: String
    var request: Blakeswap_V1_ConfirmTradeRequest {
        var value = Blakeswap_V1_ConfirmTradeRequest()
        value.requestID = requestID; value.token = token; value.revision = revision
        value.expectedWallet = profile; value.expectedNetwork = network
        return value
    }
}

@MainActor
struct TradeConfirmationJournal {
    let root: String
    private var directory: URL { URL(fileURLWithPath: root).appendingPathComponent("pending-trades", isDirectory: true) }
    private func path(profile: String, network: String) -> URL {
        let key = SHA256.hash(data: Data((profile + "|" + network).utf8)).map { String(format: "%02x", $0) }.joined()
        return directory.appendingPathComponent(key + ".json")
    }
    private func checkPrivate(_ path: URL, directory: Bool = false) throws {
        let attrs = try FileManager.default.attributesOfItem(atPath: path.path)
        guard attrs[.type] as? FileAttributeType == (directory ? .typeDirectory : .typeRegular),
              let mode = attrs[.posixPermissions] as? NSNumber, mode.intValue & 0o077 == 0 else {
            throw RPCError.message("Saved trade confirmation must be a private regular file in a private directory.")
        }
    }
    func load(profile: String, network: String) throws -> PendingTradeConfirmation? {
        let file = path(profile: profile, network: network)
        guard FileManager.default.fileExists(atPath: file.path) else { return nil }
        try checkPrivate(directory, directory: true); try checkPrivate(file)
        let data = try Data(contentsOf: file)
        guard data.count <= 16_384 else { throw RPCError.message("Saved trade confirmation is invalid.") }
        let pending = try JSONDecoder().decode(PendingTradeConfirmation.self, from: data)
        guard pending.profile == profile, pending.network == network,
              [pending.requestID, pending.token, pending.revision].allSatisfy({ $0.count == 64 && $0.allSatisfy({ $0.isHexDigit }) }),
              ["maker", "taker"].contains(pending.kind) else { throw RPCError.message("Saved trade confirmation does not match this wallet.") }
        return pending
    }
    func save(_ pending: PendingTradeConfirmation) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try checkPrivate(directory, directory: true)
        let file = path(profile: pending.profile, network: pending.network)
        if let existing = try load(profile: pending.profile, network: pending.network) {
            guard existing == pending else { throw RPCError.message("Another confirmation is already saved. Reopen the review to retry it.") }
            return
        }
        try JSONEncoder().encode(pending).write(to: file, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
    func clear(_ pending: PendingTradeConfirmation) throws {
        let file = path(profile: pending.profile, network: pending.network)
        if let saved = try load(profile: pending.profile, network: pending.network) {
            guard saved == pending else { throw RPCError.message("Saved confirmation changed; reopen the review.") }
            try FileManager.default.removeItem(at: file)
        }
    }
}

@MainActor
final class TradeReviewModel: ObservableObject {
    @Published var quote: Blakeswap_V1_TradeQuote?
    @Published var pending: PendingTradeConfirmation?
    @Published var busy = false
    @Published var error: String?
    @Published var acceptedID: String?
    @Published private(set) var acceptedKind: String?
    @Published private(set) var journalBlocked = false
    let context: TradeContext
    private let journal: TradeConfirmationJournal
    private let call: (String, Data) async throws -> Data

    init(context: TradeContext, root: String, call: ((String, Data) async throws -> Data)? = nil) {
        self.context = context
        self.journal = TradeConfirmationJournal(root: root)
        self.call = call ?? { method, payload in
            try await DaemonRPC.call(root: root, profile: context.profile, method: method, payload: payload)
        }
        do { pending = try journal.load(profile: context.profile, network: context.network) }
        catch { self.error = error.localizedDescription; journalBlocked = true }
    }
    func review(_ draft: Blakeswap_V1_TradeQuoteRequest, current: () -> TradeContext) async {
        guard !busy, pending == nil, !journalBlocked, context.matches(current()) else { return }
        busy = true; quote = nil; error = nil
        defer { busy = false }
        var request = draft
        request.expectedWallet = context.profile; request.expectedNetwork = context.network
        do {
            let data = try await call("trade.quote", request.jsonUTF8Data())
            let result = try Blakeswap_V1_TradeQuote(serializedBytes: data)
            guard !Task.isCancelled, context.matches(current()) else { return }
            guard result.wallet == context.profile, result.walletKey == context.walletKey, result.network == context.network,
                  result.kind == request.kind else { throw RPCError.message("The wallet changed while quoting. Reopen the review.") }
            quote = result
            if !result.ready { error = result.error.isEmpty ? result.funds.message : result.error }
        } catch {
            guard !Task.isCancelled, context.matches(current()) else { return }
            self.error = error.localizedDescription
        }
    }
    func back() { guard !busy, pending == nil else { return }; quote = nil; error = nil }
    func confirm(current: () -> TradeContext) async {
        guard !busy, !journalBlocked, acceptedID == nil, context.matches(current()) else { return }
        busy = true; error = nil
        defer { busy = false }
        do {
            if pending == nil {
                guard let quote, quote.ready, quote.expires > Int64(Date().timeIntervalSince1970) else { throw RPCError.message("Quote expired. Review the economics again.") }
                let id = (UUID().uuidString + UUID().uuidString).replacingOccurrences(of: "-", with: "").lowercased()
                let saved = PendingTradeConfirmation(profile: context.profile, network: context.network, requestID: id, token: quote.token, revision: quote.revision, kind: quote.kind)
                try journal.save(saved)
                pending = saved
            }
            guard let saved = pending else { return }
            let data = try await call("trade.confirm", saved.request.jsonUTF8Data())
            let result = try Blakeswap_V1_ConfirmTradeResult(serializedBytes: data)
            guard result.id == saved.requestID, result.kind == saved.kind || (result.kind.isEmpty && result.state == "rejected") else { throw RPCError.message("Confirmation response does not match the saved request. Retry the saved confirmation.") }
            switch result.state {
            case "accepted", "rejected":
                try journal.clear(saved)
                pending = nil
                guard context.matches(current()) else { return }
                if result.state == "accepted" { acceptedKind = saved.kind; acceptedID = result.id }
                else { quote = nil; error = result.error }
            case "pending":
                if context.matches(current()) { error = "Confirmation is still pending. Retry this saved request; it will not create a second trade." }
            default: throw RPCError.message("Unknown confirmation result. Retry the saved request.")
            }
        } catch {
            guard context.matches(current()) else { return }
            self.error = error.localizedDescription + (pending == nil ? "" : " The confirmation identity is saved. Retry it to learn the outcome.")
        }
    }
}

extension AppModel {
    var tradeContext: TradeContext { TradeContext(profile: profile, network: network, generation: generation, walletKey: status?.pubkey ?? "") }
}

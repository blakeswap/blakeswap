import Foundation
import XCTest
import SwiftProtobuf
@testable import Blakeswap

final class IsolatedCredentialStore: CredentialStore, @unchecked Sendable {
    private let lock = NSLock()
    private var values: [CredentialKey: Data] = [:]
    var failure: NativeSecurityError?
    func get(_ key: CredentialKey) throws -> Data { lock.lock(); defer { lock.unlock() }; if let failure { throw failure }; guard let data = values[key] else { throw NativeSecurityError.missing }; return Data(data) }
    func create(_ key: CredentialKey, password: Data) throws { lock.lock(); defer { lock.unlock() }; if let failure { throw failure }; guard values[key] == nil else { throw NativeSecurityError.exists }; values[key] = Data(password) }
    func delete(_ key: CredentialKey) throws { lock.lock(); defer { lock.unlock() }; if let failure { throw failure }; values.removeValue(forKey: key) }
}
@MainActor
final class IsolatedOwnerAuthenticator: OwnerAuthenticator {
    var failure: NativeSecurityError?
    var calls = 0
    var cancellations = 0
    var onAuthenticate: (@MainActor () async throws -> Void)?
    var onCancel: (@MainActor () -> Void)?
    func authenticate(reason: String) async throws { calls += 1; if let failure { throw failure }; try await onAuthenticate?() }
    func cancel() { cancellations += 1; onCancel?() }
}
@MainActor
func isolatedNativeSecurity() -> NativeSecurity { NativeSecurity(store: IsolatedCredentialStore(), authenticator: IsolatedOwnerAuthenticator(), initiallyAuthorized: true) }

final class NativeSecurityTests: XCTestCase {
    @MainActor
    func testPrivateCredentialFailuresRemainExplicitAndDoNotSelectOtherInstallation() async throws {
        struct Request: Encodable { let key: CredentialKey; var password: Data? }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at:root,withIntermediateDirectories:false,attributes:[.posixPermissions:0o700])
        defer { try? FileManager.default.removeItem(at:root) }
        let installation = String(repeating:"a",count:64)
        let marker = root.appendingPathComponent("installation.json")
        try Data(installation.utf8).write(to:marker)
        try FileManager.default.setAttributes([.posixPermissions:0o600],ofItemAtPath:marker.path)
        let input = Pipe(),output = Pipe(),session = UUID().uuidString
        let store = IsolatedCredentialStore()
        let security = NativeSecurity(store:store,authenticator:IsolatedOwnerAuthenticator(),initiallyAuthorized:true)
        let helper = NativePeer(input:output.fileHandleForReading,output:input.fileHandleForWriting,session:session) { _, raw in raw }
        try security.attach(root:root.path,session:session,pid:1234,input:input.fileHandleForReading,output:output.fileHandleForWriting)
        defer { security.closeConnection(); helper.close() }
        let key = CredentialKey(installation:installation,profile:"alice",record:String(repeating:"b",count:64))
        for failure in [NativeSecurityError.locked,.denied,.missing] {
            store.failure = failure
            do { _ = try await helper.call("credential.get",payload:JSONEncoder().encode(Request(key:key))); XCTFail("Failed Keychain access returned bytes") } catch { XCTAssertEqual(error as? NativeSecurityError,failure) }
            XCTAssertEqual(security.credentialFailure,failure)
        }
        store.failure = nil
        let foreign = CredentialKey(installation:String(repeating:"c",count:64),profile:key.profile,record:key.record)
        do { _ = try await helper.call("credential.get",payload:JSONEncoder().encode(Request(key:foreign))); XCTFail("Foreign installation selected") } catch { XCTAssertEqual(error as? NativeSecurityError,.changed) }
        _ = try await helper.call("credential.create",payload:JSONEncoder().encode(Request(key:key,password:Data(repeating:0x31,count:32))))
        XCTAssertNil(security.credentialFailure)
        _ = try await helper.call("credential.get",payload:JSONEncoder().encode(Request(key:key)))
        XCTAssertNil(security.credentialFailure)
    }
    @MainActor
    func testInitialCancellationNeedsExplicitRetryAndRevocationRetainsUnlock() async throws {
        let owner = IsolatedOwnerAuthenticator(); owner.failure = .cancelled
        let security = NativeSecurity(store: IsolatedCredentialStore(), authenticator: owner)
        do { try await security.unlock(); XCTFail("Cancelled initial unlock succeeded") } catch { XCTAssertEqual(error as? NativeSecurityError, .cancelled) }
        do { try await security.unlock(); XCTFail("Polling retried a cancelled prompt") } catch {}
        XCTAssertEqual(owner.calls, 1); XCTAssertFalse(security.initialAuthorized)
        owner.failure = nil; try await security.unlock(retry: true)
        XCTAssertEqual(owner.calls, 2); XCTAssertTrue(security.initialAuthorized)
        let generation = security.generation; security.revoke()
        XCTAssertGreaterThan(security.generation, generation); XCTAssertTrue(security.initialAuthorized, "Screen lock must retain existing settlement keys")
        security.closeConnection(); XCTAssertFalse(security.initialAuthorized, "Helper restart requires another unlock")
    }
    @MainActor
    func testOwnedNativeHelperCreatesNoFileAndReopensWithNewSession() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else { throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the fresh signed helper") }
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let store = IsolatedCredentialStore(), owner = IsolatedOwnerAuthenticator()
        let first = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper), security: NativeSecurity(store: store, authenticator: owner))
        var second: DaemonProcess?
        addTeardownBlock { await second?.stop(); await first.stop(); try? FileManager.default.removeItem(at: root) }
        try await first.unlockAndStart(); try await first.waitUntilReady(profile: "alice")
        let endpoint = try DaemonRPC.endpoint(root: root.path, profile: "alice")
        XCTAssertEqual(endpoint.credentialMode, "native")
        let raw = try await DaemonRPC.call(root: root.path, profile: "alice", method: "settings.get")
        let settings = try AppSettings(serializedBytes: raw)
        var prepare = Blakeswap_V1_PrepareFirstWalletRequest(); prepare.name = "Isolated native wallet"; prepare.revision = settings.revision
        let created = try Blakeswap_V1_FirstWallet(serializedBytes: await DaemonRPC.call(root: root.path, profile: "alice", method: "onboarding.prepare", payload: prepare.jsonUTF8Data()))
        XCTAssertFalse(FileManager.default.fileExists(atPath: root.appendingPathComponent("wallets/alice/vault.password").path))
        XCTAssertGreaterThanOrEqual(owner.calls, 2)
        var request = URLRequest(url: URL(string: endpoint.http + "/v1/onboarding/recovery")!)
        request.httpMethod = "POST"; request.httpBody = Data("{}".utf8); request.setValue("application/json", forHTTPHeaderField: "Content-Type"); request.setValue("Bearer " + endpoint.token, forHTTPHeaderField: "Authorization")
        let (_, response) = try await URLSession.shared.data(for: request)
        XCTAssertGreaterThanOrEqual((response as! HTTPURLResponse).statusCode, 400)
        // Exercise the real private helper and public HTTP consumption boundary,
        // with a deterministic synthetic screen-lock event between the two.
        let held = try await first.security.authorize(endpoint:endpoint,profile:"alice",method:"onboarding.get",payload:Data("{}".utf8))
        let distributed = NotificationCenter(), locked = expectation(description:"owned lock received")
        var retire: Task<Void,Never>?
        let events = SessionRevocations(workspace:NotificationCenter(),distributed:distributed) { retire = first.security.revoke(); locked.fulfill() }
        distributed.post(name:Notification.Name("com.apple.screenIsLocked"),object:nil)
        await fulfillment(of:[locked],timeout:2)
        await retire?.value
        withExtendedLifetime(events) {}
        request.setValue(try XCTUnwrap(held),forHTTPHeaderField:"X-Blakeswap-Consent")
        let (_, retiredResponse) = try await URLSession.shared.data(for:request)
        XCTAssertGreaterThanOrEqual((retiredResponse as! HTTPURLResponse).statusCode,400)
        request.setValue(nil,forHTTPHeaderField:"X-Blakeswap-Consent")
        // A pending OS reply must also become unusable, while the helper keeps
        // the loaded profile and read-only service alive.
        let began = expectation(description:"owned pending prompt")
        var waiting: CheckedContinuation<Void,Error>?
        owner.onAuthenticate = { try await withCheckedThrowingContinuation { waiting = $0; began.fulfill() } }
        owner.onCancel = { let reply = waiting; waiting = nil; reply?.resume(throwing:NativeSecurityError.cancelled) }
        let pending = Task { try await DaemonRPC.call(root:root.path,profile:"alice",method:"onboarding.get") }
        await fulfillment(of:[began],timeout:2)
        first.revokeConsent()
        do { _ = try await pending.value; XCTFail("Lock left pending OS reply usable") } catch {}
        owner.onAuthenticate = nil; owner.onCancel = nil
        _ = try await DaemonRPC.call(root:root.path,profile:"alice",method:"settings.get")
        XCTAssertTrue(first.isRunning)
        first.revokeConsent()
        owner.failure = .denied
        do { _ = try await DaemonRPC.call(root: root.path, profile: "alice", method: "onboarding.get"); XCTFail("Denied OS prompt disclosed recovery") } catch {}
        owner.failure = nil
        await first.stop()
        let nextOwner = IsolatedOwnerAuthenticator()
        let resumed = DaemonProcess(root: root.path, executable: URL(fileURLWithPath: helper), security: NativeSecurity(store: store, authenticator: nextOwner)); second = resumed
        try await resumed.unlockAndStart(); try await resumed.waitUntilReady(profile: "alice")
        let nextEndpoint = try DaemonRPC.endpoint(root: root.path, profile: "alice")
        XCTAssertNotEqual(endpoint.ownerSession, nextEndpoint.ownerSession)
        let recovery = try Blakeswap_V1_FirstWallet(serializedBytes: await DaemonRPC.call(root: root.path, profile: "alice", method: "onboarding.get"))
        XCTAssertTrue(recovery.recovery.mnemonic == created.recovery.mnemonic, "Helper restart changed wallet identity")
        XCTAssertEqual(nextOwner.calls, 2)
        let runtime = try Data(contentsOf: root.appendingPathComponent("runtime.json"))
        XCTAssertNil(runtime.range(of: Data(created.recovery.mnemonic.utf8)))
        let log = try Data(contentsOf: root.appendingPathComponent("desktop.log"))
        XCTAssertNil(log.range(of: Data(created.recovery.mnemonic.utf8)))
    }
}

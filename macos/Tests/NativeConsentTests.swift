import AppKit
import CryptoKit
import Foundation
import XCTest
@testable import Blakeswap

@MainActor
private final class PendingOwner: OwnerAuthenticator {
    var onBegin: (() -> Void)?
    private var pending: CheckedContinuation<Void, Error>?
    func authenticate(reason: String) async throws {
        try await withCheckedThrowingContinuation { continuation in
            pending = continuation; onBegin?()
        }
    }
    func allow() { let reply = pending; pending = nil; reply?.resume() }
    func cancel() { let reply = pending; pending = nil; reply?.resume(throwing: NativeSecurityError.cancelled) }
}

private actor ConsentProtocolProbe {
    let session: String
    private var challenge: ConsentChallenge?
    private var approved = false
    private(set) var approvals = 0
    private(set) var lastPreparation = Data()
    init(session: String) { self.session = session }
    func handle(_ method: String, _ data: Data) throws -> Data {
        switch method {
        case "consent.prepare":
            lastPreparation = data
            let object = try JSONSerialization.jsonObject(with: data) as! [String: Any]
            let action = ConsentAction(installation: String(repeating:"a",count:64), wallet: object["profile"] as! String, walletKey:String(repeating:"b",count:64), network:"regtest",epoch:"current",method:object["method"] as! String,digest:SHA256.hash(data:data).map { String(format:"%02x",$0) }.joined())
            let next = ConsentChallenge(id:try NativePeer.randomID(),session:session,action:action,expires:Int64(Date().timeIntervalSince1970)+60)
            challenge = next; approved = false
            return try JSONEncoder().encode(next)
        case "consent.approve":
            guard let challenge, try JSONDecoder().decode(ConsentChallenge.self,from:data) == challenge else { throw NativeSecurityError.changed }
            approved = true; approvals += 1; return Data("true".utf8)
        case "consent.revoke","consent.cancel":
            challenge = nil; approved = false; return Data("true".utf8)
        default: throw NativeSecurityError.invalid
        }
    }
    func consume(_ id: String) -> Bool {
        defer { challenge = nil; approved = false }
        return approved && challenge?.id == id
    }
}

@MainActor
private func nativeConsentFixture(_ t: XCTestCase, owner: OwnerAuthenticator) throws -> (NativeSecurity, ConsentProtocolProbe, NativePeer, DaemonEndpoint) {
    let inbound = Pipe(),outbound = Pipe(),session = UUID().uuidString
    let security = NativeSecurity(store:IsolatedCredentialStore(),authenticator:owner,initiallyAuthorized:true)
    let probe = ConsentProtocolProbe(session:session)
    let helper = NativePeer(input:outbound.fileHandleForReading,output:inbound.fileHandleForWriting,session:session) { method,raw in try await probe.handle(method,raw) }
    try security.attach(root:"/isolated-native-consent-"+session,session:session,pid:1234,input:inbound.fileHandleForReading,output:outbound.fileHandleForWriting)
    let endpoint = DaemonEndpoint(socket:"/isolated",http:"http://127.0.0.1",token:String(repeating:"c",count:64),ownerPID:1234,ownerSession:session,credentialMode:"native")
    return (security,probe,helper,endpoint)
}

final class NativeConsentTests: XCTestCase {
    @MainActor
    func testInjectedLockDuringPromptCannotApproveOrResume() async throws {
        let owner = PendingOwner(),began = expectation(description:"OS prompt began"),locked = expectation(description:"lock delivered")
        owner.onBegin = { began.fulfill() }
        let (security,probe,helper,endpoint) = try nativeConsentFixture(self,owner:owner)
        defer { security.closeConnection(); helper.close() }
        let workspace = NotificationCenter(),distributed = NotificationCenter()
        let events = SessionRevocations(workspace:workspace,distributed:distributed) { security.revoke(); locked.fulfill() }
        defer { withExtendedLifetime(events) {} }
        let pending = Task { try await security.authorize(endpoint:endpoint,profile:"alice",method:"wallet.send",payload:Data("{}".utf8)) }
        await fulfillment(of:[began],timeout:2)
        distributed.post(name:Notification.Name("com.apple.screenIsLocked"),object:nil)
        await fulfillment(of:[locked],timeout:2)
        do { _ = try await pending.value; XCTFail("Lock during prompt approved request") } catch {}
        owner.allow() // A late UI reply cannot restore the retired attempt.
        workspace.post(name:NSWorkspace.sessionDidBecomeActiveNotification,object:nil)
        let count = await probe.approvals
        XCTAssertEqual(count,0); XCTAssertTrue(security.initialAuthorized)
    }
    @MainActor
    func testInjectedLockAfterApprovalRetiresGrantWithoutRelockingSettlement() async throws {
        let owner = IsolatedOwnerAuthenticator()
        let (security,probe,helper,endpoint) = try nativeConsentFixture(self,owner:owner)
        defer { security.closeConnection(); helper.close() }
        let distributed = NotificationCenter(),delivered = expectation(description:"revocation delivered")
        var revocation: Task<Void,Never>?
        let events = SessionRevocations(workspace:NotificationCenter(),distributed:distributed) { revocation = security.revoke(); delivered.fulfill() }
        defer { withExtendedLifetime(events) {} }
        let raw = Data("{\"amount\":\"9007199254740993\",\"destination\":\"reviewed\"}".utf8)
        let approved = try await security.authorize(endpoint:endpoint,profile:"alice",method:"wallet.send",payload:raw)
        let grant = try XCTUnwrap(approved)
        let captured = await probe.lastPreparation
        let object = try JSONSerialization.jsonObject(with:captured) as! [String:Any]
        XCTAssertEqual((object["params"] as? [String:Any])?["amount"] as? String,"9007199254740993")
        distributed.post(name:Notification.Name("com.apple.screenIsLocked"),object:nil)
        await fulfillment(of:[delivered],timeout:2)
        await revocation?.value
        let consumed = await probe.consume(grant)
        XCTAssertFalse(consumed); XCTAssertTrue(security.initialAuthorized)
        XCTAssertEqual(owner.calls,1)
        // A later explicit action requires another OS decision.
        _ = try await security.authorize(endpoint:endpoint,profile:"alice",method:"wallet.send",payload:raw)
        XCTAssertEqual(owner.calls,2)
    }
    @MainActor
    func testChangedSessionAndDeniedOwnerCannotApprove() async throws {
        let owner = IsolatedOwnerAuthenticator()
        let (security,probe,helper,endpoint) = try nativeConsentFixture(self,owner:owner)
        defer { security.closeConnection(); helper.close() }
        let foreign = DaemonEndpoint(socket:endpoint.socket,http:endpoint.http,token:endpoint.token,ownerPID:endpoint.ownerPID,ownerSession:"replacement",credentialMode:"native")
        do { _ = try await security.authorize(endpoint:foreign,profile:"alice",method:"onboarding.get",payload:Data("{}".utf8)); XCTFail("Foreign helper session accepted") } catch {}
        XCTAssertEqual(owner.calls,0)
        for failure in [NativeSecurityError.cancelled,.denied,.unavailable] {
            owner.failure = failure
            do { _ = try await security.authorize(endpoint:endpoint,profile:"alice",method:"onboarding.get",payload:Data("{}".utf8)); XCTFail("Failed OS authentication accepted") } catch { XCTAssertEqual(error as? NativeSecurityError,failure) }
        }
        let count = await probe.approvals
        XCTAssertEqual(count,0)
    }
    @MainActor
    func testReplyAlreadyInFlightCannotSurviveRevocationOrOwnerReplacement() async throws {
        let owner = IsolatedOwnerAuthenticator()
        let (security,_,helper,endpoint) = try nativeConsentFixture(self,owner:owner)
        defer { security.closeConnection(); helper.close() }
        let id = try await security.authorize(endpoint:endpoint,profile:"alice",method:"onboarding.get",payload:Data("{}".utf8))
        let permission = NativeSecurityRegistry.Permission(id:try XCTUnwrap(id),security:security,generation:security.generation)
        XCTAssertNoThrow(try permission.validateReply())
        await security.revoke().value
        XCTAssertThrowsError(try permission.validateReply())
        security.closeConnection()
        XCTAssertThrowsError(try permission.validateReply())
    }
}

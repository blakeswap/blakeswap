import XCTest
import Foundation
import SwiftProtobuf
@testable import Blakeswap

final class DaemonRPCTests: XCTestCase {
    func testPrivateEndpointRequired() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let file = root.appendingPathComponent("runtime.json")
        try Data("{}".utf8).write(to: file)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: file.path)
        XCTAssertThrowsError(try DaemonRPC.endpoint(root: root.path, profile: "alice"))
    }

    func testNativeGRPCAtomicTrade() async throws {
        guard let root = ProcessInfo.processInfo.environment["BLAKESWAP_SWIFT_TEST_ROOT"] else {
            throw XCTSkip("Set BLAKESWAP_SWIFT_TEST_ROOT to the isolated external regtest fixture")
        }
        func call(_ profile: String, _ method: String, _ params: [String: Any] = [:]) async throws -> Data {
            var bound = params
            if ["offer.create", "offer.cancel", "swap.take", "pause", "regtest.mine", "regtest.faucet"].contains(method) {
                bound["expected_network"] = "regtest"
            }
            return try await DaemonRPC.call(root: root, profile: profile, method: method, params: bound)
        }
        func status(_ profile: String) async throws -> DaemonStatus {
            try DaemonStatus(serializedBytes: await call(profile, "status"))
        }
        var ready = false
        for _ in 0..<80 {
            if let settingsRaw = try? await call("alice", "settings.get"),
               let settings = try? AppSettings(serializedBytes: settingsRaw) {
                guard settings.activeNetwork == "regtest" else { XCTFail("Refusing non-regtest fixture"); return }
                if let alice = try? await status("alice"), let bob = try? await status("bob"),
                   alice.network == "regtest", bob.network == "regtest", alice.addresses.count == 2, bob.addresses.count == 2 { ready = true; break }
            }
            try await Task.sleep(nanoseconds: 250_000_000)
        }
        XCTAssertTrue(ready, "Fixture wallets did not connect")
        guard ready else { return }
        do {
            _ = try await call("alice", "offer.create", ["sell": "btc", "sell_amount": 1, "buy_amount": 1])
            XCTFail("Invalid offer accepted")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("invalid order bounds"), "Backend error was hidden: \(error.localizedDescription)")
        }
        let initial = try await status("alice")
        XCTAssertTrue(initial.ownWatchtower.npub.hasPrefix("npub1"))
        XCTAssertFalse(initial.ownWatchtower.public)
        XCTAssertEqual(initial.ownWatchtower.scripts.count, 2)
        for profile in ["alice", "bob"] {
            for chain in ["btc", "blake"] {
                _ = try await call(profile, "regtest.faucet", ["chain": chain, "amount": 100_000_000])
            }
        }
        _ = try await call("alice", "regtest.mine", ["blocks": 2])
        let offer = try Blakeswap_V1_Offer(serializedBytes: await call("alice", "offer.create", ["sell": "btc", "sell_amount": 1_000_000, "buy_amount": 2_000_000]))
        var delivered = false
        for _ in 0..<80 {
            if try await status("bob").orders.contains(where: { $0.id == offer.id }) { delivered = true; break }
            try await Task.sleep(nanoseconds: 250_000_000)
        }
        XCTAssertTrue(delivered, "Offer not delivered through the external local relay")
        guard delivered else { return }
        let taken = try Blakeswap_V1_TakeOfferResponse(serializedBytes: await call("bob", "swap.take", ["maker": offer.maker, "id": offer.id]))
        var mined = Set<String>()
        for _ in 0..<160 {
            let alice = try await status("alice"), bob = try await status("bob")
            let swaps = [alice, bob].compactMap { $0.swaps.first(where: { $0.id == taken.id }) }
            if swaps.count == 2 && swaps.allSatisfy({ $0.stage == "completed" }) {
                for swap in swaps {
                    XCTAssertEqual(swap.towerPaid, 0)
                    XCTAssertGreaterThanOrEqual(swap.longConfirmations, 2)
                    XCTAssertGreaterThanOrEqual(swap.shortConfirmations, 2)
                    XCTAssertEqual(swap.long.amount, 2_000_000)
                    XCTAssertEqual(swap.short.amount, 1_000_000)
                }
                try swaps[0].jsonUTF8Data().write(to: URL(fileURLWithPath: root).appendingPathComponent("successful-swift-trade.json"))
                print("Native gRPC trade completed: \(taken.id), long \(swaps[0].longSpend), short \(swaps[0].shortSpend)")
                return
            }
            let ids = swaps.flatMap { [$0.long.txid, $0.short.txid, $0.longSpend, $0.shortSpend] }.filter { !$0.isEmpty }
            if ids.contains(where: { !mined.contains($0) }) {
                _ = try await call("alice", "regtest.mine", ["blocks": 2])
                mined.formUnion(ids)
            }
            try await Task.sleep(nanoseconds: 500_000_000)
        }
        XCTFail("Native gRPC trade did not settle")
    }
}

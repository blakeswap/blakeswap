import Foundation
import XCTest
import SwiftProtobuf
@testable import Blakeswap

final class OnboardingTests: XCTestCase {
    func testNativeFirstLaunchBackupRestartAndBothRestoreMethods() async throws {
        guard let helper = ProcessInfo.processInfo.environment["BLAKESWAP_TEST_HELPER"] else {
            throw XCTSkip("Set BLAKESWAP_TEST_HELPER to the built desktop helper")
        }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let root = directory.appendingPathComponent("created").path
        var processes: [Process] = []
        defer { for process in processes where process.isRunning { process.terminate(); process.waitUntilExit() } }
        func call<M: Message>(_ root: String, _ method: String, _ request: M) async throws -> Data {
            try await DaemonRPC.call(root: root, profile: "alice", method: method, payload: request.jsonUTF8Data())
        }
        func start(_ root: String) async throws -> AppSettings {
            let process = Process(); process.executableURL = URL(fileURLWithPath: helper)
            process.arguments = ["desktop", "--data-dir", root]
            process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
            try process.run(); processes.append(process)
            for _ in 0..<100 {
                if let raw = try? await call(root, "settings.get", Google_Protobuf_Empty()) { return try AppSettings(serializedBytes: raw) }
                if !process.isRunning { throw RPCError.message("Onboarding helper exited") }
                try await Task.sleep(nanoseconds: 100_000_000)
            }
            throw RPCError.message("Onboarding helper did not expose its private API")
        }
        func confirm(_ root: String, _ first: Blakeswap_V1_FirstWallet) async throws -> AppSettings {
            let words = first.recovery.mnemonic.split(separator: " ")
            var request = Blakeswap_V1_ConfirmFirstWalletRequest(); request.revision = first.settings.revision
            request.words = first.backupWordPositions.map { String(words[Int($0) - 1]) }
            return try AppSettings(serializedBytes: await call(root, "onboarding.confirm", request))
        }
        let initial = try await start(root)
        XCTAssertEqual(initial.onboardingStage, "wallet")
        XCTAssertFalse(FileManager.default.fileExists(atPath: root + "/wallets"))
        var request = Blakeswap_V1_PrepareFirstWalletRequest(); request.name = "First wallet"; request.revision = initial.revision
        let first = try Blakeswap_V1_FirstWallet(serializedBytes: await call(root, "onboarding.prepare", request))
        XCTAssertEqual(first.settings.onboardingStage, "backup")
        XCTAssertEqual(first.recovery.mnemonic.split(separator: " ").count, 24)
        XCTAssertFalse(FileManager.default.fileExists(atPath: root + "/wallets/alice/mainnet/state.db"))
        let status = try DaemonStatus(serializedBytes: await call(root, "status", Google_Protobuf_Empty()))
        XCTAssertTrue(status.addresses.isEmpty)
        XCTAssertFalse(try String(contentsOfFile: root + "/settings.json", encoding: .utf8).contains(first.recovery.mnemonic))
        processes[0].terminate(); processes[0].waitUntilExit()
        let resumed = try await start(root)
        XCTAssertEqual(resumed.onboardingStage, "backup")
        let recovered = try Blakeswap_V1_FirstWallet(serializedBytes: await call(root, "onboarding.get", Google_Protobuf_Empty()))
        XCTAssertTrue(recovered.recovery.mnemonic == first.recovery.mnemonic, "Restart changed the saved recovery phrase")
        var export = Blakeswap_V1_ExportFirstWalletRequest()
        export.revision = resumed.revision; export.path = directory.appendingPathComponent("wallet.blakeswap").path
        export.password = "disposable test backup password"
        _ = try await call(root, "onboarding.export", export)
        XCTAssertNil(try Data(contentsOf: URL(fileURLWithPath: export.path)).range(of: Data(first.recovery.mnemonic.utf8)))
        var connected = try await confirm(root, recovered)
        XCTAssertEqual(connected.onboardingStage, "connect")
        // Complete setup against unreachable loopback fixtures; never use public services in this test.
        for i in connected.environments.indices {
            connected.environments[i].relays = ["ws://127.0.0.1:1"]
            for chain in ["btc", "blake"] {
                var node = NodeSettings(); node.kind = "electrum"; node.url = "tcp://127.0.0.1:1"
                connected.environments[i].nodes[chain] = node
            }
        }
        let finished = try AppSettings(serializedBytes: await call(root, "onboarding.finish", connected))
        XCTAssertEqual(finished.onboardingStage, "")
        do { _ = try await call(root, "onboarding.get", Google_Protobuf_Empty()); XCTFail("Setup secret remained available after completion") } catch {}
        processes[1].terminate(); processes[1].waitUntilExit()
        let reopened = try await start(root)
        XCTAssertEqual(reopened.onboardingStage, "")
        processes[2].terminate(); processes[2].waitUntilExit()

        let phraseRoot = directory.appendingPathComponent("phrase").path
        let phraseSettings = try await start(phraseRoot)
        request.name = "Phrase restored"; request.revision = phraseSettings.revision
        request.mnemonic = first.recovery.mnemonic
        let phrase = try Blakeswap_V1_FirstWallet(serializedBytes: await call(phraseRoot, "onboarding.prepare", request))
        XCTAssertTrue(phrase.recovery.mnemonic == first.recovery.mnemonic, "Phrase restore changed keys")
        let phraseConfirmed = try await confirm(phraseRoot, phrase)
        XCTAssertEqual(phraseConfirmed.onboardingStage, "connect")

        let backupRoot = directory.appendingPathComponent("backup").path
        let backupSettings = try await start(backupRoot)
        request = Blakeswap_V1_PrepareFirstWalletRequest(); request.name = "Backup restored"
        request.revision = backupSettings.revision; request.backupPath = export.path; request.backupPassword = export.password
        let backup = try Blakeswap_V1_FirstWallet(serializedBytes: await call(backupRoot, "onboarding.prepare", request))
        XCTAssertEqual(backup.settings.onboardingStage, "connect")
        XCTAssertFalse(backup.hasRecovery)
        XCTAssertTrue(FileManager.default.fileExists(atPath: backupRoot + "/wallets/alice/mainnet/state.db"))
    }
}

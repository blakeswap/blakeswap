import XCTest
@testable import Blakeswap

final class AppModelTests: XCTestCase {
    @MainActor
    func testNetworkSwitchRejectsMismatchedAddressesAndOldGeneration() {
        let model = AppModel()
        var main = AppSettings(); main.activeNetwork = "mainnet"; main.revision = 1
        var status = DaemonStatus(); status.name = "alice"; status.network = "mainnet"; status.addresses = ["btc": "main-address"]
        XCTAssertTrue(model.acceptSnapshot(status, settings: main, profile: "alice", generation: model.generation))
        let before = model.generation
        var test = main; test.activeNetwork = "testnet"; test.revision = 2
        XCTAssertFalse(model.acceptSnapshot(status, settings: test, profile: "alice", generation: before))
        XCTAssertNil(model.status)
        XCTAssertEqual(model.network, "testnet")
        XCTAssertFalse(model.acceptSnapshot(status, settings: main, profile: "alice", generation: before))
        XCTAssertNil(model.status)
        status.network = "testnet"; status.addresses = ["btc": "test-address"]
        XCTAssertTrue(model.acceptSnapshot(status, settings: test, profile: "alice", generation: model.generation))
        XCTAssertEqual(model.status?.addresses["btc"], "test-address")
        let pending = model.generation
        model.invalidateSnapshot() // Settings save invalidates requests already in flight.
        XCTAssertFalse(model.acceptSnapshot(status, settings: test, profile: "alice", generation: pending))
        XCTAssertNil(model.status)
    }

    @MainActor
    func testProfileRoundTripAndOlderSettingsCannotPublishStaleSnapshot() {
        let model = AppModel()
        var settings = AppSettings(); settings.activeNetwork = "regtest"; settings.revision = 4
        var status = DaemonStatus(); status.name = "alice"; status.network = "regtest"
        XCTAssertTrue(model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation))
        let pending = model.generation
        model.selectProfile("bob"); model.selectProfile("alice")
        XCTAssertFalse(model.acceptSnapshot(status, settings: settings, profile: "alice", generation: pending))
        settings.revision = 3
        XCTAssertFalse(model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation))
        XCTAssertEqual(model.settings?.revision, 4)
        XCTAssertNil(model.status)
    }
}

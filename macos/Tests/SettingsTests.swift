import XCTest
@testable import Blakeswap

final class SettingsTests: XCTestCase {
    func testRescueFeeDefaultAndNetworkDraftRoundTrip() throws {
        var settings = AppSettings()
        for network in ["mainnet", "testnet", "regtest"] {
            var environment = EnvironmentSettings(); environment.network = network
            settings.environments.append(environment)
        }
        XCTAssertEqual(settings.environments[2].rescueFeeBasisPoints, 50)
        XCTAssertEqual(settings.environments[2].rescueFeeBps, 0, "Displaying the default must not modify the draft")
        for bps: Int64 in [1, 125, 1000] {
            settings.environments[2].rescueFeeBasisPoints = bps
            let restored = try AppSettings(serializedBytes: settings.serializedData())
            XCTAssertEqual(restored.environments[2].rescueFeeBasisPoints, bps)
            XCTAssertEqual(restored.environments[0].rescueFeeBasisPoints, 50)
            XCTAssertEqual(restored.environments[1].rescueFeeBasisPoints, 50)
        }
        XCTAssertEqual(percentage(125), "1.25%")
    }

    @MainActor
    func testPartialChainReadinessAndNetworkSwitch() {
        let model = AppModel()
        var settings = AppSettings(); settings.activeNetwork = "regtest"; settings.revision = 1
        var status = DaemonStatus(); status.name = "alice"; status.network = "regtest"
        model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation)
        XCTAssertNil(model.status?.heights["btc"])
        XCTAssertNil(model.status?.heights["blake"])
        status.heights["btc"] = 123
        model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation)
        XCTAssertEqual(model.status?.heights["btc"], 123)
        XCTAssertNil(model.status?.heights["blake"])
        status.heights["blake"] = 0 // A reported genesis height differs from an absent observation.
        model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation)
        XCTAssertEqual(model.status?.heights["blake"], 0)
        settings.activeNetwork = "mainnet"; settings.revision = 2
        model.acceptSnapshot(status, settings: settings, profile: "alice", generation: model.generation)
        XCTAssertNil(model.status?.heights["btc"])
        XCTAssertNil(model.status?.heights["blake"])
    }
}

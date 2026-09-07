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

    func testNumericRescueFeeInputUsesBasisPointPrecisionAndBounds() {
        var environment = EnvironmentSettings()
        XCTAssertEqual(environment.rescueFeePercent, 0.5)
        environment.rescueFeePercent = 1.234
        XCTAssertEqual(environment.rescueFeeBasisPoints, 123)
        environment.rescueFeePercent = 0
        XCTAssertEqual(environment.rescueFeeBasisPoints, 1)
        environment.rescueFeePercent = 20
        XCTAssertEqual(environment.rescueFeeBasisPoints, 1000)
        environment.rescueFeePercent = .infinity
        XCTAssertEqual(environment.rescueFeeBasisPoints, 1000)
    }

    func testOrderedFallbacksAndReadinessRoundTrip() throws {
        var primary = NodeSettings(); primary.kind = "rpc"; primary.url = "http://127.0.0.1:19443"; primary.cookie = "/tmp/fixture.cookie"
        var secondary = NodeSettings(); secondary.kind = "electrum"; secondary.url = "ssl://secondary.example:50002"; secondary.certificateSha256 = String(repeating: "ab", count: 32)
        primary.fallbacks = [secondary]
        let restored = try NodeSettings(serializedBytes: primary.serializedData())
        XCTAssertEqual(restored.fallbacks, [secondary]); XCTAssertEqual(restored.cookie, primary.cookie)
        var status = DaemonStatus(); status.heights["btc"] = 123
        var connection = Blakeswap_V1_ChainConnection(); connection.ready = false; connection.lastObservation = 100
        status.connections["btc"] = connection
        let snapshot = try DaemonStatus(serializedBytes: status.serializedData())
        XCTAssertEqual(snapshot.heights["btc"], 123)
        XCTAssertEqual(snapshot.connections["btc"]?.ready, false)
        XCTAssertEqual(snapshot.connections["btc"]?.lastObservation, 100)
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

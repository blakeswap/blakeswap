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

extension AppModelTests {
    func testOfferBalancesIncludeFeeAndRejectMissingOrInvalidAmounts() {
        var status = DaemonStatus(); status.pubkey = "maker"; status.fundingFee = 2_000
        for chain in ["btc", "blake"] {
            XCTAssertFalse(status.canSell(chain))
            XCTAssertNotNil(status.offerValidation(sell: chain, sellAmount: "100000", buyAmount: "100000"))
            status.balances[chain] = 101_999
            XCTAssertFalse(status.canSell(chain))
            XCTAssertNotNil(status.offerValidation(sell: chain, sellAmount: "100000", buyAmount: "100000"))
            status.balances[chain] = 102_000
            XCTAssertTrue(status.canSell(chain))
            XCTAssertNil(status.offerValidation(sell: chain, sellAmount: "100000", buyAmount: "100000"))
            XCTAssertNotNil(status.offerValidation(sell: chain, sellAmount: "100001", buyAmount: "100000"))
            for value in ["", "1.5", "-1", "99999", "10000000001", "9223372036854775808"] {
                XCTAssertNotNil(status.offerValidation(sell: chain, sellAmount: value, buyAmount: "100000"))
                XCTAssertNotNil(status.offerValidation(sell: chain, sellAmount: "100000", buyAmount: value))
            }
        }
    }

    func testOrderFiltersOnlyIncludeOpenOrdersForSelectedWallet() {
        var status = DaemonStatus(); status.pubkey = "alice"
        for maker in ["alice", "bob"] {
            for state in ["open", "reserved", "filled", "cancelled"] {
                var offer = Order(); offer.id = state; offer.maker = maker; offer.status = state
                status.orders.append(offer)
            }
        }
        XCTAssertEqual(OrderFilter.all.orders(in: status).count, 2)
        XCTAssertEqual(OrderFilter.mine.orders(in: status).map(\.maker), ["alice"])
        XCTAssertEqual(OrderFilter.others.orders(in: status).map(\.maker), ["bob"])
        XCTAssertEqual(Set(OrderFilter.all.orders(in: status).map(\.bookID)).count, 2)
        status.pubkey = "bob"
        XCTAssertEqual(OrderFilter.mine.orders(in: status).map(\.maker), ["bob"])
    }
}

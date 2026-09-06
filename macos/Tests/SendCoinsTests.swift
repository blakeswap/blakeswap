import XCTest
@testable import Blakeswap

final class SendCoinsTests: XCTestCase {
    private func coin(reserved: Bool = false, confirmations: Int32 = 6) -> Blakeswap_V1_WalletCoin {
        var coin = Blakeswap_V1_WalletCoin()
        coin.chain = "btc"; coin.txid = String(repeating: "ab", count: 32)
        coin.amount = 100_000; coin.confirmations = confirmations; coin.reserved = reserved
        return coin
    }
    func testManualFeeAndExactCoinControlArePreservedForConfirmation() throws {
        let coin = coin()
        let plan = try SendPlan(context: SendContext(profile: "alice", network: "mainnet", chain: "btc"), destination: "bc1qrecipient", amount: "90000", fee: "1234", coins: [coin], selection: [coin.outpointID])
        XCTAssertEqual(plan.request.amount, 90_000)
        XCTAssertEqual(plan.request.fee, 1_234)
        XCTAssertEqual(plan.change, 8_766)
        XCTAssertEqual(plan.request.inputs.count, 1)
        XCTAssertEqual(plan.request.inputs[0].txid, coin.txid)
        XCTAssertEqual(plan.request.expectedNetwork, "mainnet")
    }
    func testLockedUnconfirmedAndWrongChainCoinsCannotBeReviewed() {
        let context = SendContext(profile: "alice", network: "mainnet", chain: "btc")
        for coin in [coin(reserved: true), coin(confirmations: 1)] {
            XCTAssertThrowsError(try SendPlan(context: context, destination: "bc1qrecipient", amount: "90000", fee: "1000", coins: [coin], selection: [coin.outpointID]))
        }
        var other = coin(); other.chain = "blake"
        XCTAssertThrowsError(try SendPlan(context: context, destination: "bc1qrecipient", amount: "90000", fee: "1000", coins: [other], selection: [other.outpointID]))
        let selected = coin()
        XCTAssertThrowsError(try SendPlan(context: context, destination: "bc1qrecipient", amount: "98900", fee: "1000", coins: [selected], selection: [selected.outpointID]))
        XCTAssertThrowsError(try SendPlan(context: context, destination: "bc1qrecipient", amount: "90000", fee: "-1", coins: [selected], selection: [selected.outpointID]))
    }
}

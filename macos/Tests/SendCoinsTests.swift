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
    func testFeeReviewBindsWalletNetworkAmountsAndSelectedInputs() {
        var point = Blakeswap_V1_Outpoint(); point.txid = "original"; point.vout = 0
        let original = feeReviewKey(profile: "alice", network: "mainnet", kind: "send", chain: "btc", amount: "90000", destination: "recipient", fee: "1000", automatic: true, inputs: [point])
        let changed = feeReviewKey(profile: "bob", network: "mainnet", kind: "send", chain: "btc", amount: "90000", destination: "recipient", fee: "1000", automatic: true, inputs: [point])
        XCTAssertNotEqual(original, changed)
        point.vout = 1
        XCTAssertNotEqual(original, feeReviewKey(profile: "alice", network: "mainnet", kind: "send", chain: "btc", amount: "90000", destination: "recipient", fee: "1000", automatic: true, inputs: [point]))
        var q = Blakeswap_V1_FeeQuote(); q.fee = 1234; q.estimate.rateSatKvb = 6539; q.estimate.timestamp = 123
        let review = FeeReview(key: original, quote: q, automatic: true)
        XCTAssertEqual(review.fundingParams["funding_fee"] as? Int64, 1234)
        XCTAssertEqual(review.fundingParams["rate_sat_kvb"] as? Int64, 6539)
        XCTAssertEqual(review.fundingParams["owner_fee_cap"] as? Int, 20000)
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

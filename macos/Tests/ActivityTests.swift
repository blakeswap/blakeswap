import XCTest
import SwiftProtobuf
@testable import Blakeswap

@MainActor
final class ActivityTests: XCTestCase {
    private let context = TradeContext(profile: "alice", network: "regtest", generation: 1, walletKey: "key")
    private func row(_ id: String) -> Blakeswap_V1_ActivityRecord {
        var value = Blakeswap_V1_ActivityRecord(); value.id = id; value.wallet = context.profile; value.network = context.network
        value.chain = "blake"; value.amount = 9007199254740993; value.status = "confirmed"; value.createdSource = "unknown"
        return value
    }
    func testEmptyLoadingFailureAndRetryStates() async throws {
        var continuation: CheckedContinuation<Data, Error>?
        let model = ActivityModel(context: context, root: "/unused") { _, _ in try await withCheckedThrowingContinuation { continuation = $0 } }
        let task = Task { await model.load(current: { self.context }) }
        while continuation == nil { await Task.yield() }
        XCTAssertEqual(model.phase, .loading); XCTAssertFalse(model.isEmpty)
        continuation?.resume(throwing: RPCError.message("Unavailable")); await task.value
        XCTAssertEqual(model.phase, .failed); XCTAssertEqual(model.error, "Unavailable")
        continuation = nil
        let retry = Task { await model.load(current: { self.context }) }
        while continuation == nil { await Task.yield() }
        var page = Blakeswap_V1_ActivityPage(); page.snapshot = "snapshot"
        continuation?.resume(returning: try page.serializedData()); await retry.value
        XCTAssertEqual(model.phase, .loaded); XCTAssertTrue(model.isEmpty); XCTAssertNil(model.error)
    }
    func testPaginationUsesFrozenScopeAndDetailsPreserveExactValues() async throws {
        var calls = 0
        let first = row("new"), second = row("old")
        let model = ActivityModel(context: context, root: "/unused") { method, data in
            XCTAssertEqual(method, "activity.list")
            let request = try Blakeswap_V1_ActivityQuery(jsonUTF8Data: data)
            XCTAssertEqual(request.expectedWallet, self.context.profile); XCTAssertEqual(request.expectedNetwork, self.context.network)
            XCTAssertEqual(request.chain, "blake")
            var page = Blakeswap_V1_ActivityPage(); page.snapshot = "snapshot"; page.total = 2
            if calls == 0 { XCTAssertEqual(request.cursor, 0); XCTAssertTrue(request.snapshot.isEmpty); page.records = [first]; page.nextCursor = 1 }
            else { XCTAssertEqual(request.cursor, 1); XCTAssertEqual(request.snapshot, "snapshot"); page.records = [second] }
            calls += 1; return try page.serializedData()
        }
        model.filters.chain = "blake"
        await model.load(current: { self.context }); await model.load(more: true, current: { self.context })
        XCTAssertEqual(model.records.map(\.id), ["new", "old"]); XCTAssertEqual(model.nextCursor, 0)
        model.select(second); XCTAssertEqual(model.selected?.amount, 9007199254740993)
        XCTAssertEqual(activityDate(model.selected?.createdAt ?? 0), "Unknown")
        XCTAssertEqual(ActivityDestination.order("o"), ActivityDestination(page: "Market", anchor: "order/o"))
        XCTAssertEqual(ActivityDestination.swap("s"), ActivityDestination(page: "Swaps", anchor: "swap/s"))
        XCTAssertEqual(ActivityDestination.send("p"), ActivityDestination(page: "Wallet", anchor: "send/p"))
        XCTAssertNil(ActivityDestination.swap(""))
    }
    func testDelayedHistoryCannotCrossWalletNetworkGenerationOrFilters() async throws {
        for change in ["wallet", "network", "generation", "filters"] {
            var current = context
            var continuation: CheckedContinuation<Data, Error>?
            let model = ActivityModel(context: context, root: "/unused") { _, _ in try await withCheckedThrowingContinuation { continuation = $0 } }
            let task = Task { await model.load(current: { current }) }
            while continuation == nil { await Task.yield() }
            switch change {
            case "wallet": current = TradeContext(profile: "bob", network: context.network, generation: 2, walletKey: "other")
            case "network": current = TradeContext(profile: context.profile, network: "mainnet", generation: 2, walletKey: context.walletKey)
            case "generation": current = TradeContext(profile: context.profile, network: context.network, generation: 2, walletKey: context.walletKey)
            default: model.filters.chain = "btc"
            }
            var page = Blakeswap_V1_ActivityPage(); page.snapshot = "snapshot"; page.records = [row("late")]
            continuation?.resume(returning: try page.serializedData()); await task.value
            XCTAssertTrue(model.records.isEmpty)
        }
    }
    func testCSVExportUsesEveryPageOfTheDisplayedSnapshot() async throws {
        var exported: [UInt32] = []
        let item = row("new")
        let model = ActivityModel(context: context, root: "/unused") { method, data in
            let request = try Blakeswap_V1_ActivityQuery(jsonUTF8Data: data)
            if method == "activity.list" {
                var page = Blakeswap_V1_ActivityPage(); page.snapshot = "snapshot"; page.total = 2; page.records = [item]; page.nextCursor = 1
                return try page.serializedData()
            }
            XCTAssertEqual(request.snapshot, "snapshot"); exported.append(request.cursor)
            var chunk = Blakeswap_V1_ActivityExport(); chunk.snapshot = "snapshot"; chunk.total = 2
            if request.cursor == 0 { chunk.csv = "id,amount_sats\nnew,9007199254740993\n"; chunk.nextCursor = 1 }
            else { chunk.csv = "old,100001\n" }
            return try chunk.serializedData()
        }
        await model.load(current: { self.context })
        let csv = await model.export(current: { self.context })
        XCTAssertEqual(csv, "id,amount_sats\nnew,9007199254740993\nold,100001\n")
        XCTAssertEqual(exported, [0, 1]); XCTAssertFalse(model.exporting)
    }
    func testExplorersAreExplicitAndBoundToEachChainAndNetwork() {
        var settings = AppSettings()
        var regtest = EnvironmentSettings(); regtest.network = "regtest"; regtest.explorers = ["btc": "http://127.0.0.1:8080/tx/{txid}"]
        var mainnet = EnvironmentSettings(); mainnet.network = "mainnet"; mainnet.explorers = ["blake": "https://example.invalid/blake/{txid}"]
        settings.environments = [regtest, mainnet]
        var record = row("id"); let txid = String(repeating: "a", count: 64)
        XCTAssertNil(activityExplorer(record: record, txid: txid, settings: settings))
        record.chain = "btc"
        XCTAssertEqual(activityExplorer(record: record, txid: txid, settings: settings)?.host, "127.0.0.1")
        record.network = "mainnet"; XCTAssertNil(activityExplorer(record: record, txid: txid, settings: settings))
        record.chain = "blake"; XCTAssertEqual(activityExplorer(record: record, txid: txid, settings: settings)?.path, "/blake/" + txid)
        XCTAssertNil(activityExplorer(record: record, txid: "../secret", settings: settings))
        settings.environments[1].explorers["blake"] = "javascript:{txid}"
        XCTAssertNil(activityExplorer(record: record, txid: txid, settings: settings))
    }
}

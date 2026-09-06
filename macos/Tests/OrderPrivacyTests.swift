import XCTest
@testable import Blakeswap

final class OrderPrivacyTests: XCTestCase {
    func testProtectionLabelBelongsOnlyToMaker() {
        var order = Order(); order.maker = "maker"
        XCTAssertEqual(order.protectionLabel(viewer: "maker"), "No protection")
        XCTAssertNil(order.protectionLabel(viewer: "taker"))
        XCTAssertNil(order.protectionLabel(viewer: ""))
        order.towerBps = 125
        XCTAssertEqual(order.protectionLabel(viewer: "maker"), "Watchtower: 1.25% only if used")
        XCTAssertNil(order.protectionLabel(viewer: "taker"))
    }
}

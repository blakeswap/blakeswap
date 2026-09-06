import XCTest
import CoreImage
@testable import Blakeswap

final class ReceiveQRCodeTests: XCTestCase {
    func testQRCodeRoundTripsExactCurrentAddress() throws {
        let detector = try XCTUnwrap(CIDetector(ofType: CIDetectorTypeQRCode, context: CIContext(), options: [CIDetectorAccuracy: CIDetectorAccuracyHigh]))
        for address in ["bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh", "bcrt1qexampleaddressforanotherwallet00000000000"] {
            let image = try XCTUnwrap(ReceiveQRCode.image(address: address))
            let enlarged = CIImage(cgImage: image).transformed(by: CGAffineTransform(scaleX: 8, y: 8))
            let codes = detector.features(in: enlarged).compactMap { ($0 as? CIQRCodeFeature)?.messageString }
            XCTAssertEqual(codes, [address], "The QR must encode the displayed address without a wrong-chain URI")
        }
        XCTAssertNil(ReceiveQRCode.image(address: ""))
    }
}

import SwiftUI
import CoreImage.CIFilterBuiltins
import AppKit

// Encode the literal address: both chains use the same Bech32 prefixes, so a
// Bitcoin payment URI would incorrectly identify a Blake2b payment as Bitcoin.
enum ReceiveQRCode {
    static func image(address: String) -> CGImage? {
        guard !address.isEmpty else { return nil }
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(address.utf8)
        filter.correctionLevel = "M"
        guard let output = filter.outputImage else { return nil }
        // Four white modules on every side provide the required quiet zone.
        let bounds = output.extent.insetBy(dx: -4, dy: -4)
        let background = CIImage(color: .white).cropped(to: bounds)
        let padded = output.composited(over: background)
        return CIContext().createCGImage(padded, from: bounds)
    }
}

struct ReceiveAddressView: View {
    let chain: String
    let network: String
    let address: String
    @State private var showingQR = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Receive \(symbol(chain))").font(.headline)
            Text(address).font(.system(.body, design: .monospaced)).textSelection(.enabled)
            HStack {
                Button("Copy address") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(address, forType: .string)
                }
                Button(showingQR ? "Hide QR code" : "Show QR code") { showingQR.toggle() }
                    .accessibilityLabel("\(showingQR ? "Hide" : "Show") \(symbol(chain)) receive QR code")
            }.disabled(address.isEmpty)
            if showingQR {
                VStack(spacing: 10) {
                    Text("\(symbol(chain)) · \(network == "testnet" ? "Testnet4" : network.capitalized)").font(.headline)
                    if let image = ReceiveQRCode.image(address: address) {
                        Image(decorative: image, scale: 1)
                            .interpolation(.none).resizable().scaledToFit()
                            .frame(width: 240, height: 240)
                            .accessibilityLabel("Receive \(symbol(chain)) at \(address)")
                    } else {
                        Text("QR code unavailable. Copy the address above.")
                    }
                    Text("Send only \(symbol(chain)) on \(network == "testnet" ? "Testnet4" : network.capitalized).")
                        .font(.caption).foregroundStyle(.secondary)
                }.padding(.vertical, 8)
            }
            Text("A new address appears after a payment confirms. Previous addresses remain part of your wallet.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }
}

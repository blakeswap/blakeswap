import SwiftUI
import AppKit

struct ChainHeightIndicator: View {
    let chain: String
    let height: UInt32?

    var body: some View {
        HStack(spacing: 6) {
            Text(symbol(chain))
            if let height {
                Text("#\(height)")
            } else {
                ProgressView().controlSize(.mini)
                    .accessibilityLabel("Connecting to \(symbol(chain))")
            }
        }.accessibilityIdentifier("\(chain)-height")
    }
}

struct RegtestConnectionHelp: View {
    var body: some View {
        Text("Regtest needs running Bitcoin and Bitcoin Blake2b nodes. From a Blakeswap source checkout, run make regtest-nodes for both, or make regtest-btc / make regtest-blake for one chain. Leave the cookie field empty to discover these nodes automatically. For other nodes, set the endpoint and choose their .cookie file.")
            .font(.caption).foregroundStyle(.secondary)
    }
}

struct RPCCookieField: View {
    @Binding var path: String
    let chain: String

    var body: some View {
        HStack {
            TextField("RPC cookie file (empty for local regtest discovery)", text: $path).textFieldStyle(.roundedBorder)
                .accessibilityIdentifier("\(chain)-rpc-cookie")
            Button("Choose…") {
                let panel = NSOpenPanel()
                panel.canChooseFiles = true
                panel.canChooseDirectories = false
                panel.allowsMultipleSelection = false
                panel.showsHiddenFiles = true
                panel.message = "Choose the RPC cookie for your \(symbol(chain)) node."
                if !path.isEmpty { panel.directoryURL = URL(fileURLWithPath: path).deletingLastPathComponent() }
                if panel.runModal() == .OK, let url = panel.url { path = url.path }
            }.accessibilityLabel("Choose \(symbol(chain)) RPC cookie file")
        }
    }
}

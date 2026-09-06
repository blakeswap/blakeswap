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
        Text("Regtest needs running Bitcoin and Bitcoin Blake2b nodes. Set each node’s RPC endpoint and choose the .cookie file in its data directory’s regtest folder. Default paths are suggestions; your nodes may store their data elsewhere.")
            .font(.caption).foregroundStyle(.secondary)
    }
}

struct RPCCookieField: View {
    @Binding var path: String
    let chain: String

    var body: some View {
        HStack {
            TextField("RPC cookie file", text: $path).textFieldStyle(.roundedBorder)
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

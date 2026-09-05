import SwiftUI

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel
    @State private var draft: AppSettings
    @State private var editing: String
    @State private var checks: [String: String] = [:]
    @State private var checking: String?
    init(settings: AppSettings) { _draft = State(initialValue: settings); _editing = State(initialValue: settings.activeNetwork) }
    private var index: Int { draft.environments.firstIndex(where: { $0.network == editing }) ?? 0 }
    private var environment: EnvironmentSettings { draft.environments[index] }
    private func node(_ chain: String) -> Binding<NodeSettings> {
        Binding(get: { draft.environments[index].nodes[chain] ?? NodeSettings() }, set: { draft.environments[index].nodes[chain] = $0 })
    }
    private func relay(_ position: Int) -> Binding<String> {
        Binding(get: { environment.relays.count > position ? environment.relays[position] : "" }, set: { value in
            while draft.environments[index].relays.count <= position { draft.environments[index].relays.append("") }
            draft.environments[index].relays[position] = value
        })
    }
    private func towerScript(_ chain: String) -> Binding<String> {
        Binding(get: { environment.tower.scripts[chain] ?? "" }, set: { draft.environments[index].tower.scripts[chain] = $0 })
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 22) {
            Picker("Trading network", selection: $draft.activeNetwork) {
                Text("Regtest").tag("regtest"); Text("Testnet4").tag("testnet"); Text("Mainnet").tag("mainnet")
            }.accessibilityIdentifier("active-network")
            Picker("Configure", selection: $editing) {
                Text("Regtest").tag("regtest"); Text("Testnet4").tag("testnet"); Text("Mainnet").tag("mainnet")
            }.pickerStyle(.segmented).accessibilityIdentifier("settings-environment")
            Group {
                ForEach(["btc", "blake"], id: \.self) { chain in
                    GroupBox(symbol(chain)) {
                        VStack(alignment: .leading, spacing: 12) {
                            Picker("Backend", selection: node(chain).kind) {
                                Text("Electrum").tag("electrum"); Text("Full-node RPC").tag("rpc")
                            }
                            TextField("Endpoint", text: node(chain).url).textFieldStyle(.roundedBorder).accessibilityIdentifier("\(chain)-endpoint")
                            if environment.nodes[chain]?.kind == "rpc" {
                                TextField("RPC cookie file", text: node(chain).cookie).textFieldStyle(.roundedBorder)
                            } else {
                                TextField("Certificate SHA256 pin (optional)", text: node(chain).certificateSha256).textFieldStyle(.roundedBorder)
                            }
                            HStack {
                                Button("Test connection") {
                                    checking = chain
                                    let selected = editing
                                    let endpoint = environment.nodes[chain] ?? NodeSettings()
                                    Task { let result = await model.checkNode(network: selected, chain: chain, node: endpoint); checks["\(selected)-\(chain)"] = result; checking = nil }
                                }.disabled(checking != nil || (environment.nodes[chain]?.url.isEmpty ?? true))
                                if checking == chain { ProgressView().controlSize(.small) }
                            }
                            if let result = checks["\(editing)-\(chain)"] { Text(result).font(.caption).textSelection(.enabled) }
                            if editing == "testnet" && chain == "blake" && (environment.nodes[chain]?.url.isEmpty ?? true) {
                                Text("No verified public Blake2b Testnet4 indexer is available. Configure your own Electrum server or Knots RPC node.").font(.caption).foregroundStyle(.secondary)
                            }
                        }.padding(10)
                    }
                }
                if environment.nodes.values.contains(where: { $0.kind == "electrum" }) {
                    Text("Electrum operators are trusted for the current chain and complete transaction history. The daemon checks transaction data, block hashes and inclusion proofs; it does not validate the full chain. Use your own full node for consensus validation.").font(.caption).foregroundStyle(.secondary)
                }
                GroupBox("Nostr relays") {
                    VStack(spacing: 10) { ForEach(0..<3) { position in TextField("wss://relay.example", text: relay(position)).textFieldStyle(.roundedBorder) } }.padding(10)
                }
                GroupBox("Watchtower") {
                    VStack(alignment: .leading, spacing: 10) {
                        TextField("Nostr public key", text: $draft.environments[index].tower.pubkey).textFieldStyle(.roundedBorder)
                        TextField("Fee (basis points)", value: $draft.environments[index].tower.bps, format: .number).textFieldStyle(.roundedBorder)
                        TextField("BTC payout script (hex)", text: towerScript("btc")).textFieldStyle(.roundedBorder)
                        TextField("BLAKE payout script (hex)", text: towerScript("blake")).textFieldStyle(.roundedBorder)
                        Text("100 basis points = 1%. A provider is required to offer watchtower protection; the fee is paid only when its delayed rescue transaction confirms.").font(.caption).foregroundStyle(.secondary)
                    }.padding(10)
                }
            }
            if editing == "regtest" { Text("Regtest requires separately running BTC and Blake2b nodes. The app does not start or stop them.").font(.caption).foregroundStyle(.secondary) }
            HStack {
                Button("Reload") { Task { await model.loadSettings() } }.disabled(model.busy)
                Spacer()
                Button("Save settings") {
                    var saved = draft
                    for i in saved.environments.indices { saved.environments[i].relays = saved.environments[i].relays.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty } }
                    Task { await model.saveSettings(saved) }
                }.buttonStyle(.borderedProminent).disabled(model.busy).accessibilityIdentifier("save-settings")
            }
        }
    }
}

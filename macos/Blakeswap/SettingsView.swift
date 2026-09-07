import SwiftUI
import AppKit

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel
    @State private var draft: AppSettings
    @State private var editing: String
    @State private var checks: [String: String] = [:]
    @State private var checking: String?
    @State private var watchtowerNpub = ""
    @State private var newWalletName = ""
    init(settings: AppSettings) { _draft = State(initialValue: settings); _editing = State(initialValue: settings.activeNetwork) }
    private var index: Int { draft.environments.firstIndex(where: { $0.network == editing }) ?? 0 }
    private var environment: EnvironmentSettings { draft.environments[index] }
    private func node(_ chain: String) -> Binding<NodeSettings> {
        Binding(get: { draft.environments[index].nodes[chain] ?? NodeSettings() }, set: { draft.environments[index].nodes[chain] = $0 })
    }
    private func fallback(_ chain: String, _ position: Int) -> Binding<NodeSettings> {
        Binding(get: { draft.environments[index].nodes[chain]?.fallbacks[position] ?? NodeSettings() }, set: { draft.environments[index].nodes[chain]?.fallbacks[position] = $0 })
    }
    private func endpointHealth(_ chain: String) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            if model.status?.network == editing, let connection = model.status?.connections[chain] {
                Text(connection.ready ? "Observations ready" : "Observations unavailable · last values may be stale").font(.caption).foregroundStyle(connection.ready ? Color.secondary : Color.orange)
                if connection.lastObservation > 0 { Text("Last complete observation: \(Date(timeIntervalSince1970: TimeInterval(connection.lastObservation)).formatted())").font(.caption2) }
                ForEach(Array(connection.sources.endpoints.enumerated()), id: \.offset) { _, endpoint in
                    Text("\(endpoint.active ? "Active" : "Standby"): \(endpoint.url)").font(.caption2).textSelection(.enabled)
                    if !endpoint.error.isEmpty { Text(endpoint.error).font(.caption2).foregroundStyle(.orange).textSelection(.enabled) }
                    if endpoint.retryAfter > 0 { Text("Retry after \(Date(timeIntervalSince1970: TimeInterval(endpoint.retryAfter)).formatted(date: .omitted, time: .standard))").font(.caption2) }
                }
                if connection.sources.failovers > 0 { Text("Automatic failovers: \(connection.sources.failovers)").font(.caption2) }
            }
        }
    }
    private func fallbackSettings(_ chain: String) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            ForEach(0..<(environment.nodes[chain]?.fallbacks.count ?? 0), id: \.self) { position in
                Divider()
                HStack {
                    Text("Fallback \(position + 1)").font(.headline)
                    Spacer()
                    if position > 0 { Button("Move up") { draft.environments[index].nodes[chain]?.fallbacks.swapAt(position, position - 1) } }
                    Button("Remove") { draft.environments[index].nodes[chain]?.fallbacks.remove(at: position) }
                }
                Picker("Backend", selection: fallback(chain, position).kind) { Text("Electrum").tag("electrum"); Text("Full-node RPC").tag("rpc") }
                TextField("Fallback endpoint", text: fallback(chain, position).url).textFieldStyle(.roundedBorder).accessibilityIdentifier("\(chain)-fallback-\(position)")
                if environment.nodes[chain]?.fallbacks[position].kind == "rpc" { RPCCookieField(path: fallback(chain, position).cookie, chain: chain) }
                else { TextField("Certificate SHA256 pin (optional)", text: fallback(chain, position).certificateSha256).textFieldStyle(.roundedBorder) }
                Button("Test fallback") {
                    let selected = editing
                    let endpoint = environment.nodes[chain]?.fallbacks[position] ?? NodeSettings()
                    let key = "\(selected)-\(chain)-\(position)"
                    checking = key
                    Task {
                        let result = await model.checkNode(network: selected, chain: chain, node: endpoint)
                        if selected == editing, environment.nodes[chain]?.fallbacks.indices.contains(position) == true, environment.nodes[chain]?.fallbacks[position] == endpoint { checks[key] = result }
                        checking = nil
                    }
                }.disabled(checking != nil)
                if let result = checks["\(editing)-\(chain)-\(position)"] { Text(result).font(.caption).textSelection(.enabled) }
            }
            Button("Add fallback endpoint") {
                var endpoint = NodeSettings(); endpoint.kind = "electrum"
                draft.environments[index].nodes[chain]?.fallbacks.append(endpoint)
            }.disabled((environment.nodes[chain]?.fallbacks.count ?? 0) >= 3)
            Text("Fallbacks are tried in order when the active server fails. Each server must validate for this chain and agree with the last observed history. Conflicting history requires investigation; multiple servers are not a consensus quorum.").font(.caption).foregroundStyle(.secondary)
            endpointHealth(chain)
        }
    }
    private func relay(_ position: Int) -> Binding<String> {
        Binding(get: { environment.relays.count > position ? environment.relays[position] : "" }, set: { value in
            while draft.environments[index].relays.count <= position { draft.environments[index].relays.append("") }
            draft.environments[index].relays[position] = value
        })
    }
    private var watchtowers: [Blakeswap_V1_Tower] { model.status?.network == editing ? model.status?.watchtowers ?? [] : [] }
    private var favorites: [String] { environment.favoriteWatchtowers }
    private func toggleFavorite(_ tower: Blakeswap_V1_Tower) {
        if favorites.contains(tower.npub) { draft.environments[index].favoriteWatchtowers.removeAll { $0 == tower.npub } }
        else { draft.environments[index].favoriteWatchtowers.append(tower.npub) }
    }
    private var watchtowerSettings: some View {
        GroupBox("Watchtowers") {
            VStack(alignment: .leading, spacing: 14) {
                Text("Your wallet serves as a watchtower while the app is open. Payout addresses and scripts are generated by your wallet.").font(.caption).foregroundStyle(.secondary)
                HStack {
                    Text("Rescue fee:")
                    TextField("Rescue fee", value: $draft.environments[index].rescueFeePercent, format: .number.precision(.fractionLength(2)))
                        .textFieldStyle(.roundedBorder).frame(width: 90)
                        .accessibilityIdentifier("rescue-fee")
                        .accessibilityLabel("Rescue fee percent")
                    Text("%")
                }
                Text("0.01%–10.00%, in 0.01% steps. Applies to all your wallets on this network; paid only when a rescue confirms. Save settings to update your quote. Accepted rescue jobs keep their agreed fee.").font(.caption).foregroundStyle(.secondary)
                Toggle("Show my wallets in the public watchtower list", isOn: $draft.environments[index].publicWatchtower)
                    .accessibilityIdentifier("public-watchtower")
                Text("Off by default. Share your npub directly for private discovery, or opt in to announce all your wallets as watchtowers on this network’s relays. Save settings to apply.").font(.caption).foregroundStyle(.secondary)
                if model.status?.network == editing, let own = model.status?.ownWatchtower, !own.npub.isEmpty {
                    Text("Your npub · \(editing == "testnet" ? "Testnet4" : editing.capitalized)").font(.headline)
                    Text(own.npub).font(.caption.monospaced()).textSelection(.enabled)
                    HStack {
                        Button("Copy my npub") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(own.npub, forType: .string) }.accessibilityIdentifier("copy-watchtower-npub")
                        Text("Current quote: \(percentage(own.bps))").font(.caption).foregroundStyle(.secondary)
                    }
                } else { Text("Select this trading network and connect your wallet to see its npub.").font(.caption).foregroundStyle(.secondary) }
                Divider()
                Text("Favorite watchtowers").font(.headline)
                HStack {
                    TextField("Watchtower npub", text: $watchtowerNpub).textFieldStyle(.roundedBorder).accessibilityIdentifier("watchtower-npub")
                    Button("Look up & add") {
                        let value = watchtowerNpub.trimmingCharacters(in: .whitespacesAndNewlines)
                        let selected = editing
                        Task {
                            if await model.command("tower.resolve", ["pubkey": value]), selected == editing {
                                if !favorites.contains(value) { draft.environments[index].favoriteWatchtowers.append(value) }
                                watchtowerNpub = ""
                            }
                        }
                    }.disabled(model.busy || watchtowerNpub.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || model.status?.network != editing)
                }
                ForEach(favorites, id: \.self) { npub in
                    HStack {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(watchtowers.first(where: { $0.npub == npub })?.label ?? "Awaiting signed quote").font(.callout)
                            Text(npub).font(.caption2.monospaced()).textSelection(.enabled)
                        }
                        Spacer()
                        Button("Remove") { draft.environments[index].favoriteWatchtowers.removeAll { $0 == npub } }
                    }
                }
                Text("Save favorites to select them when creating offers. Private lookups are encrypted; the provider must be reachable through a shared relay.").font(.caption).foregroundStyle(.secondary)
                Divider()
                Text("Public watchtowers").font(.headline)
                Text("Discovered automatically on this network's configured relays. A signed announcement identifies the provider; it does not guarantee availability.").font(.caption).foregroundStyle(.secondary)
                if watchtowers.filter({ $0.public }).isEmpty { Text("No public watchtowers discovered yet.").foregroundStyle(.secondary).font(.callout) }
                ForEach(watchtowers.filter { $0.public }) { tower in
                    HStack {
                        VStack(alignment: .leading, spacing: 4) {
                            Text(tower.label).font(.callout)
                            Text(tower.npub).font(.caption2.monospaced()).textSelection(.enabled)
                        }
                        Spacer()
                        Button(favorites.contains(tower.npub) ? "Remove favorite" : "Add favorite") { toggleFavorite(tower) }
                    }
                }
            }.padding(10)
        }
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 22) {
            GroupBox("Wallets") {
                VStack(alignment: .leading, spacing: 12) {
                    Text("Each wallet has its own keys and balances on every network. All wallets keep running while the app is open.").font(.caption).foregroundStyle(.secondary)
                    ForEach(draft.wallets.indices, id: \.self) { i in
                        HStack {
                            TextField("Wallet name", text: $draft.wallets[i].name).textFieldStyle(.roundedBorder).accessibilityIdentifier("wallet-name-\(draft.wallets[i].id)")
                            if draft.wallets[i].id == model.profile { Text("Selected").font(.caption).foregroundStyle(.secondary) }
                        }
                    }
                    Text("Edit names and save settings to rename wallets. Keys and balances stay the same.").font(.caption).foregroundStyle(.secondary)
                    HStack {
                        TextField("New wallet name", text: $newWalletName).textFieldStyle(.roundedBorder).accessibilityIdentifier("new-wallet-name")
                        Button("Create wallet") { Task { await model.createWallet(name: newWalletName) } }
                            .disabled(model.busy || newWalletName.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || draft != model.settings || draft.wallets.count >= 20)
                            .accessibilityIdentifier("create-wallet")
                    }
                    if draft != model.settings { Text("Save or reload your settings edits before creating a wallet.").font(.caption).foregroundStyle(.secondary) }
                }.padding(10)
            }
            Picker("Trading network", selection: $draft.activeNetwork) {
                Text("Regtest").tag("regtest"); Text("Testnet4").tag("testnet"); Text("Mainnet").tag("mainnet")
            }.accessibilityIdentifier("active-network")
                .onChange(of: draft.activeNetwork) { _, network in editing = network }
            Picker("Configure", selection: $editing) {
                Text("Regtest").tag("regtest"); Text("Testnet4").tag("testnet"); Text("Mainnet").tag("mainnet")
            }.pickerStyle(.segmented).accessibilityIdentifier("settings-environment")
            if editing == "regtest" { RegtestConnectionHelp() }
            Group {
                ForEach(["btc", "blake"], id: \.self) { chain in
                    GroupBox(symbol(chain)) {
                        VStack(alignment: .leading, spacing: 12) {
                            Picker("Backend", selection: node(chain).kind) {
                                Text("Electrum").tag("electrum"); Text("Full-node RPC").tag("rpc")
                            }
                            TextField("Endpoint", text: node(chain).url).textFieldStyle(.roundedBorder).accessibilityIdentifier("\(chain)-endpoint")
                            if environment.nodes[chain]?.kind == "rpc" {
                                RPCCookieField(path: node(chain).cookie, chain: chain)
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
                            fallbackSettings(chain)
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
                watchtowerSettings
            }
            HStack {
                Button("Reload") { Task { if let saved = await model.loadSettings() { draft = saved; editing = saved.activeNetwork; checks = [:] } } }.disabled(model.busy)
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

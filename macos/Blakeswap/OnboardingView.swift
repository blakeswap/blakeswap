import SwiftUI
import AppKit
import SwiftProtobuf
import UniformTypeIdentifiers

struct OnboardingView: View {
    @EnvironmentObject private var model: AppModel
    @State private var draft: AppSettings
    @State private var mode: String?
    @State private var name = "My wallet"
    @State private var mnemonic = ""
    @State private var backupPath = ""
    @State private var password = ""
    @State private var exportPassword = ""
    @State private var phraseAcknowledged = false
    @State private var showingWords = true
    @State private var answers = ["", "", ""]
    @State private var checking = false
    @State private var checkResults: [String: String] = [:]
    init(settings: AppSettings) { _draft = State(initialValue: settings) }
    private var stage: String { model.settings?.onboardingStage ?? "wallet" }
    private var step: Int { stage == "wallet" ? 0 : stage == "backup" ? 1 : 2 }
    private var environmentIndex: Int { draft.environments.firstIndex { $0.network == draft.activeNetwork } ?? 0 }
    private func node(_ chain: String) -> Binding<NodeSettings> {
        Binding(get: { draft.environments[environmentIndex].nodes[chain] ?? NodeSettings() }, set: { draft.environments[environmentIndex].nodes[chain] = $0; checkResults = [:] })
    }
    private func relay(_ position: Int) -> Binding<String> {
        Binding(get: { let relays = draft.environments[environmentIndex].relays; return relays.indices.contains(position) ? relays[position] : "" }, set: { value in
            while draft.environments[environmentIndex].relays.count <= position { draft.environments[environmentIndex].relays.append("") }
            draft.environments[environmentIndex].relays[position] = value
        })
    }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 28) {
                HStack(spacing: 10) {
                    Image(systemName: "arrow.triangle.2.circlepath").foregroundStyle(mint).font(.title2)
                    Text("blakeswap").font(.system(size: 23, weight: .semibold, design: .rounded))
                    Spacer()
                    Text("LET’S GET STARTED").font(.system(size: 10, weight: .semibold)).tracking(1.5).foregroundStyle(.secondary)
                }.padding(.bottom, 12)
                HStack(spacing: 16) {
                    ForEach(Array(["Your wallet", "Backup", "Connections"].enumerated()), id: \.offset) { index, title in
                        HStack(spacing: 8) {
                            Text(index < step ? "✓" : "\(index + 1)")
                                .font(.system(size: 11, weight: .bold)).frame(width: 25, height: 25)
                                .background(index <= step ? mint.opacity(0.14) : Color.white.opacity(0.04), in: Circle())
                            Text(title).font(.system(size: 12, weight: .medium))
                        }.foregroundStyle(index <= step ? mint : .secondary)
                        if index < 2 { Rectangle().fill(Color.white.opacity(0.08)).frame(height: 1) }
                    }
                }.accessibilityElement(children: .combine).accessibilityLabel("Step \(step + 1) of 3")
                if stage == "wallet" { walletStep }
                else if stage == "backup" { backupStep }
                else { connectionsStep }
                if model.busy { ProgressView().controlSize(.small) }
                if let notice = model.notice { Text(notice).font(.callout).foregroundStyle(mint).textSelection(.enabled).accessibilityIdentifier("setup-notice") }
                if let error = model.connectionError { Text(error).font(.caption).foregroundStyle(.secondary) }
            }.frame(maxWidth: 700, alignment: .leading).padding(48).frame(maxWidth: .infinity)
        }.background(Color(red: 0.065, green: 0.078, blue: 0.10)).accessibilityIdentifier("onboarding")
    }
    private func title(_ heading: String, _ subtitle: String) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(heading).font(.system(size: 30, weight: .semibold, design: .rounded))
            Text(subtitle).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
    private func choice(_ label: String, _ detail: String, _ icon: String, _ value: String) -> some View {
        Button { mode = value; model.notice = nil } label: {
            HStack(spacing: 18) {
                Image(systemName: icon).font(.title2).foregroundStyle(mint).frame(width: 44, height: 44).background(mint.opacity(0.08), in: RoundedRectangle(cornerRadius: 12))
                VStack(alignment: .leading, spacing: 6) {
                    Text(label).font(.headline).foregroundStyle(.white)
                    Text(detail).font(.callout).foregroundStyle(.secondary)
                }
                Spacer()
                Image(systemName: "arrow.right").foregroundStyle(mint)
            }.padding(22).background(panel, in: RoundedRectangle(cornerRadius: 16))
        }.buttonStyle(.plain).disabled(model.busy).accessibilityIdentifier("setup-choose-\(value)")
    }
    private var walletStep: some View {
        VStack(alignment: .leading, spacing: 22) {
            if mode == nil {
                title("Welcome to Blakeswap", "Set up a wallet for Bitcoin and Bitcoin Blake2b. You hold the keys and control your funds.")
                choice("Create a new wallet", "Start fresh, then back up your recovery phrase.", "plus", "create")
                choice("Restore a wallet", "Use your recovery phrase or an encrypted wallet backup.", "arrow.counterclockwise", "restore")
            } else {
                title(mode == "create" ? "Create your wallet" : "Restore your wallet", "Give your wallet a name. You can change it later in Settings.")
                VStack(alignment: .leading, spacing: 16) {
                    TextField("Wallet name", text: $name).textFieldStyle(.roundedBorder).accessibilityIdentifier("setup-wallet-name")
                    if mode != "create" {
                        HStack(spacing: 10) {
                            restoreTab("Recovery phrase", "restore")
                            restoreTab("Backup file", "file")
                        }
                        if mode == "restore" {
                            SecureField("BIP39 recovery phrase", text: $mnemonic).textFieldStyle(.roundedBorder).accessibilityIdentifier("setup-restore-phrase")
                            Text("The phrase restores your keys on both chains. Choose a state backup instead if you need to recover pending swaps and rescue transactions.").font(.caption).foregroundStyle(.secondary)
                            Toggle("I understand that a phrase does not restore pending swap state", isOn: $phraseAcknowledged).font(.callout)
                        } else {
                            HStack {
                                Button("Choose backup file") {
                                    let panel = NSOpenPanel(); panel.canChooseDirectories = false; panel.allowsMultipleSelection = false
                                    if panel.runModal() == .OK { backupPath = panel.url?.path ?? "" }
                                }.accessibilityIdentifier("setup-choose-backup")
                                Text(backupPath.isEmpty ? "No file selected" : URL(fileURLWithPath: backupPath).lastPathComponent).font(.caption).foregroundStyle(.secondary)
                            }
                            SecureField("Backup password", text: $password).textFieldStyle(.roundedBorder).accessibilityIdentifier("setup-restore-password")
                            Text("Use the password for this backup. For a state backup saved by an older app version, use the original wallet’s vault.password file contents.").font(.caption).foregroundStyle(.secondary)
                        }
                    }
                }.padding(22).background(panel, in: RoundedRectangle(cornerRadius: 16))
                HStack {
                    Button("Back") { mode = nil; mnemonic = ""; password = ""; model.notice = nil }.disabled(model.busy)
                    Spacer()
                    Button(mode == "create" ? "Create wallet" : "Restore wallet") {
                        var request = Blakeswap_V1_PrepareFirstWalletRequest()
                        request.name = name.trimmingCharacters(in: .whitespacesAndNewlines)
                        request.revision = model.settings?.revision ?? 0
                        if mode == "restore" { request.mnemonic = mnemonic.trimmingCharacters(in: .whitespacesAndNewlines) }
                        if mode == "file" { request.backupPath = backupPath; request.backupPassword = password }
                        Task { if await model.setupAction("onboarding.prepare", request: request) { mnemonic = ""; password = "" } }
                    }.buttonStyle(MintButton()).disabled(model.busy || name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || (mode == "restore" && (mnemonic.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || !phraseAcknowledged)) || (mode == "file" && (backupPath.isEmpty || password.isEmpty)))
                        .accessibilityIdentifier("setup-prepare")
                }
            }
        }
    }
    private func restoreTab(_ label: String, _ value: String) -> some View {
        Button { mode = value; model.notice = nil } label: {
            Text(label).font(.callout.weight(.medium)).padding(.horizontal, 12).padding(.vertical, 9)
                .foregroundStyle(mode == value ? mint : .secondary)
                .background(mode == value ? mint.opacity(0.1) : .clear, in: RoundedRectangle(cornerRadius: 8))
        }.buttonStyle(.plain).disabled(model.busy)
    }
    private var backupStep: some View {
        VStack(alignment: .leading, spacing: 22) {
            title("Back up your wallet", "Write down these words in order and keep them somewhere private. Anyone with this phrase can control your wallet.")
            if let first = model.setupWallet {
                if showingWords {
                    let words = first.recovery.mnemonic.split(separator: " ").map(String.init)
                    LazyVGrid(columns: Array(repeating: GridItem(.flexible(), alignment: .leading), count: 3), spacing: 14) {
                        ForEach(Array(words.enumerated()), id: \.offset) { index, word in
                            HStack(spacing: 10) {
                                Text("\(index + 1)").font(.caption.monospaced()).foregroundStyle(.secondary).frame(width: 20)
                                Text(word).font(.system(.body, design: .monospaced))
                            }.padding(10).frame(maxWidth: .infinity, alignment: .leading).background(Color.white.opacity(0.025), in: RoundedRectangle(cornerRadius: 8))
                        }
                    }.padding(18).background(panel, in: RoundedRectangle(cornerRadius: 16)).privacySensitive()
                    DisclosureGroup("Save an encrypted backup file (optional)") {
                        VStack(alignment: .leading, spacing: 12) {
                            SecureField("Choose a backup password (16+ characters)", text: $exportPassword).textFieldStyle(.roundedBorder)
                            Button("Save encrypted backup") {
                                let panel = NSSavePanel(); panel.nameFieldStringValue = "Blakeswap-wallet.blakeswap"
                                guard panel.runModal() == .OK, let url = panel.url else { return }
                                var request = Blakeswap_V1_ExportFirstWalletRequest(); request.path = url.path; request.password = exportPassword; request.revision = model.settings?.revision ?? 0
                                Task { if await model.setupAction("onboarding.export", request: request) { exportPassword = "" } }
                            }.disabled(model.busy || exportPassword.count < 16)
                        }.padding(.top, 12)
                    }.font(.callout)
                    HStack { Spacer(); Button("I’ve saved my recovery phrase") { showingWords = false; model.notice = nil }.buttonStyle(MintButton()).disabled(model.busy).accessibilityIdentifier("setup-saved-phrase") }
                } else {
                    Text("Check your backup").font(.title3.weight(.semibold))
                    Text("Enter these three words from your saved phrase.").font(.callout).foregroundStyle(.secondary)
                    HStack(spacing: 14) {
                        ForEach(Array(first.backupWordPositions.enumerated()), id: \.offset) { index, position in
                            VStack(alignment: .leading, spacing: 8) {
                                Text("Word \(position)").font(.caption).foregroundStyle(.secondary)
                                SecureField("Word \(position)", text: $answers[index]).textFieldStyle(.roundedBorder).accessibilityIdentifier("setup-backup-word-\(position)")
                            }
                        }
                    }
                    HStack {
                        Button("Show phrase again") { showingWords = true; answers = ["", "", ""] }.disabled(model.busy)
                        Spacer()
                        Button("Confirm backup") {
                            var request = Blakeswap_V1_ConfirmFirstWalletRequest(); request.revision = model.settings?.revision ?? 0; request.words = answers
                            Task { _ = await model.setupAction("onboarding.confirm", request: request) }
                        }.buttonStyle(MintButton()).disabled(model.busy || answers.contains { $0.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }).accessibilityIdentifier("setup-confirm-backup")
                    }
                }
            } else {
                Text("Your wallet is saved. Resume its backup to continue setup.").foregroundStyle(.secondary)
                Button("Show recovery phrase") { Task { _ = await model.setupAction("onboarding.get", request: Google_Protobuf_Empty()) } }.buttonStyle(MintButton()).disabled(model.busy)
            }
        }
    }
    private var connectionsStep: some View {
        VStack(alignment: .leading, spacing: 22) {
            title("Connect your wallet", "Choose a network and the servers your wallet will use. You can update these connections later in Settings.")
            HStack(spacing: 12) {
                ForEach(["mainnet", "testnet", "regtest"], id: \.self) { network in
                    Button { draft.activeNetwork = network; checkResults = [:] } label: {
                        VStack(alignment: .leading, spacing: 8) {
                            Text(network == "testnet" ? "Testnet4" : network.capitalized).font(.headline)
                            Text(network == "mainnet" ? "Real funds" : network == "testnet" ? "Test coins" : "Local development").font(.caption).foregroundStyle(.secondary)
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(18)
                            .foregroundStyle(draft.activeNetwork == network ? mint : .white)
                            .background(draft.activeNetwork == network ? mint.opacity(0.08) : panel, in: RoundedRectangle(cornerRadius: 12))
                            .overlay { RoundedRectangle(cornerRadius: 12).strokeBorder(draft.activeNetwork == network ? mint.opacity(0.4) : .clear, lineWidth: 1) }
                    }.buttonStyle(.plain).disabled(model.busy).accessibilityIdentifier("setup-network-\(network)")
                }
            }
            ForEach(["btc", "blake"], id: \.self) { chain in
                VStack(alignment: .leading, spacing: 12) {
                    Text(chain == "btc" ? "Bitcoin" : "Bitcoin Blake2b").font(.headline)
                    Picker("Connection", selection: node(chain).kind) { Text("Electrum").tag("electrum"); Text("Full-node RPC").tag("rpc") }
                    TextField("Server endpoint", text: node(chain).url).textFieldStyle(.roundedBorder).accessibilityIdentifier("setup-\(chain)-endpoint")
                    if node(chain).wrappedValue.kind == "rpc" { TextField("RPC cookie file", text: node(chain).cookie).textFieldStyle(.roundedBorder) }
                    else { DisclosureGroup("Certificate pin (optional)") { TextField("Certificate SHA256", text: node(chain).certificateSha256).textFieldStyle(.roundedBorder) }.font(.caption) }
                    if let result = checkResults[chain] { Text(result).font(.caption).foregroundStyle(.secondary) }
                }.padding(20).background(panel, in: RoundedRectangle(cornerRadius: 14))
            }
            DisclosureGroup("Nostr relays") { VStack(spacing: 10) { ForEach(0..<3) { position in TextField("wss://relay.example", text: relay(position)).textFieldStyle(.roundedBorder) } }.padding(.top, 12) }.font(.callout)
            Text("Electrum servers supply chain observations. Use a full node you trust for consensus validation. Your watchtower is private by default.").font(.caption).foregroundStyle(.secondary)
            HStack {
                Button(checking ? "Checking connections…" : "Test connections") {
                    let selected = draft.activeNetwork
                    let nodes = draft.environments[environmentIndex].nodes
                    checking = true
                    Task {
                        for chain in ["btc", "blake"] {
                            let result = await model.checkNode(network: selected, chain: chain, node: nodes[chain] ?? NodeSettings())
                            if draft.activeNetwork == selected && draft.environments[environmentIndex].nodes == nodes { checkResults[chain] = result }
                        }
                        checking = false
                    }
                }.disabled(checking || model.busy)
                Spacer()
                Button("Open my wallet") {
                    var saved = draft
                    for i in saved.environments.indices { saved.environments[i].relays = saved.environments[i].relays.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty } }
                    Task { _ = await model.setupAction("onboarding.finish", request: saved) }
                }.buttonStyle(MintButton()).disabled(model.busy || draft.environments[environmentIndex].nodes.values.contains { $0.url.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty }).accessibilityIdentifier("setup-finish")
            }
        }
    }
}

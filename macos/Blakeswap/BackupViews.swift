import AppKit
import SwiftUI

struct WalletBackupControls: View {
    @EnvironmentObject var model: AppModel
    let status: DaemonStatus
    @State private var exporting = false
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if status.hasRecovery { RecoveryProgressView(progress: status.recovery) }
            if !status.backup.reminder.isEmpty { Text(status.backup.reminder).font(.callout).foregroundStyle(.orange) }
            if status.backup.lastExportAt > 0 {
                Text("Last portable export: \(Date(timeIntervalSince1970: TimeInterval(status.backup.lastExportAt)).formatted())").font(.caption).foregroundStyle(.secondary)
            }
            HStack {
                Button("Reveal recovery phrase") { Task { await model.command("wallet.recovery") } }
                Button("Export portable backup…") { exporting = true }.accessibilityIdentifier("wallet-portable-export")
                ImportBackupButton()
            }.disabled(model.busy)
            Text("A phrase restores keys. A state backup also preserves random swap preimages, signed rescue transactions and pending messages.").font(.caption).foregroundStyle(.secondary)
        }.sheet(isPresented: $exporting) { PortableExportView() }
    }
}

struct RecoveryProgressView: View {
    let progress: Blakeswap_V1_RecoveryProgress
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(progress.state == "ready" ? "Ready to trade" : "Recovery in progress", systemImage: progress.state == "ready" ? "checkmark.shield" : "clock.arrow.circlepath")
                .font(.headline).foregroundStyle(progress.state == "ready" ? Color.green : Color.orange)
                .accessibilityIdentifier("wallet-recovery-state")
            ForEach(Array(progress.issues.enumerated()), id: \.offset) { _, issue in
                Text(issue.id.isEmpty ? issue.reason : "\(issue.kind.capitalized) \(issue.id.prefix(12)): \(issue.reason)").font(.callout)
            }
            Text("\(progress.quarantinedOffers) old orders and \(progress.quarantinedMessages) queued publications remain quarantined.").font(.caption)
            Text(progress.coverage).font(.caption).foregroundStyle(.secondary)
        }
    }
}

struct ImportBackupButton: View {
    @EnvironmentObject var model: AppModel
    @State private var importing = false
    var body: some View {
        Button("Import backup…") { importing = true }.disabled(model.busy).accessibilityIdentifier("wallet-portable-import")
            .sheet(isPresented: $importing) { PortableImportView() }
    }
}

struct PortableExportView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    @State private var password = ""
    @State private var confirmation = ""
    @State private var allWallets = false
    @State private var path = ""
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Export portable state backup").font(.title2.bold())
            Text("Include this wallet’s state on Bitcoin and Blake2b across all three networks. The file contains receive indexes, pending sends, swap secrets and signed recovery transactions.")
            Toggle("Include every wallet profile", isOn: $allWallets)
            SecureField("Choose a backup password (16+ characters)", text: $password).textFieldStyle(.roundedBorder)
            SecureField("Confirm backup password", text: $confirmation).textFieldStyle(.roundedBorder)
            HStack {
                Button("Choose destination…") {
                    let panel = NSSavePanel(); panel.nameFieldStringValue = "blakeswap-\(Int(Date().timeIntervalSince1970)).blakeswap"
                    if panel.runModal() == .OK { path = panel.url?.path ?? "" }
                }
                Text(path.isEmpty ? "No destination selected" : URL(fileURLWithPath: path).lastPathComponent).font(.caption)
            }
            Text("Use this chosen password to restore the file on another installation. Keep it separately; an existing archive is never overwritten.").font(.caption).foregroundStyle(.secondary)
            if let notice = model.notice { Text(notice).foregroundStyle(.orange) }
            HStack {
                Button("Cancel") { password = ""; confirmation = ""; dismiss() }.disabled(model.busy)
                Spacer()
                Button("Export encrypted backup") {
                    Task { if await model.exportBackup(path: path, password: password, allWallets: allWallets) { password = ""; confirmation = ""; dismiss() } }
                }.buttonStyle(.borderedProminent).disabled(model.busy || path.isEmpty || password.utf8.count < 16 || password != confirmation)
            }
        }.padding(28).frame(width: 520).disabled(model.busy)
    }
}

struct PortableImportView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    @State private var path = ""
    @State private var password = ""
    @State private var name = ""
    @State private var selected = ""
    @State private var contents: Blakeswap_V1_BackupContents?
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text("Import a wallet backup").font(.title2.bold())
            Text("The imported wallet gets a new isolated profile. Existing wallets stay in place; duplicate identities are refused.")
            HStack {
                Button("Choose backup…") {
                    let panel = NSOpenPanel(); panel.canChooseDirectories = false; panel.allowsMultipleSelection = false
                    if panel.runModal() == .OK { path = panel.url?.path ?? ""; contents = nil; selected = "" }
                }
                Text(path.isEmpty ? "No file selected" : URL(fileURLWithPath: path).lastPathComponent).font(.caption)
            }
            SecureField("Backup password", text: $password).textFieldStyle(.roundedBorder)
                .onChange(of: password) { _, _ in contents = nil; selected = "" }
            Text("Portable files use their chosen export password. Legacy .db files use the original vault.password contents.").font(.caption).foregroundStyle(.secondary)
            Button("Inspect encrypted backup") {
                Task {
                    if let inspected = await model.inspectBackup(path: path, password: password) {
                        contents = inspected; selected = inspected.wallets.first?.sourceWalletID ?? ""; name = inspected.wallets.first?.name ?? "Recovered wallet"
                    }
                }
            }.disabled(path.isEmpty || password.isEmpty)
            if let contents {
                Picker("Wallet in backup", selection: $selected) {
                    ForEach(contents.wallets, id: \.sourceWalletID) { wallet in Text("\(wallet.name) · \(wallet.networks.joined(separator: ", "))").tag(wallet.sourceWalletID) }
                }
                TextField("New profile name", text: $name).textFieldStyle(.roundedBorder)
                Text(contents.legacy ? "Legacy snapshot date is unknown." : "Created \(Date(timeIntervalSince1970: TimeInterval(contents.createdAt)).formatted())").font(.caption)
                Text(contents.warning).font(.callout).foregroundStyle(.secondary)
                if contents.wallets.count > 1 { Text("Import one profile at a time from this archive.").font(.caption) }
            }
            if let notice = model.notice { Text(notice).foregroundStyle(.orange) }
            HStack {
                Button("Cancel") { password = ""; dismiss() }
                Spacer()
                Button("Import into new profile") {
                    Task { if await model.importBackup(path: path, password: password, sourceWallet: selected, name: name) { password = ""; dismiss() } }
                }.buttonStyle(.borderedProminent).disabled(contents == nil || selected.isEmpty || name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
            }
        }.padding(28).frame(width: 560).disabled(model.busy)
    }
}

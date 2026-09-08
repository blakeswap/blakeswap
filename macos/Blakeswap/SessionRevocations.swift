import AppKit
import Foundation

@MainActor
final class SessionRevocations {
    private let workspace: NotificationCenter
    private let injectedDistributed: NotificationCenter?
    private var workspaceTokens: [NSObjectProtocol] = []
    private var distributedTokens: [NSObjectProtocol] = []
    init(workspace: NotificationCenter = NSWorkspace.shared.notificationCenter,
         distributed: NotificationCenter? = nil, revoke: @escaping @MainActor () -> Void) {
        self.workspace = workspace; injectedDistributed = distributed
        let receive: @Sendable (Notification) -> Void = { _ in Task { @MainActor in revoke() } }
        for name in [NSWorkspace.willSleepNotification, NSWorkspace.sessionDidResignActiveNotification, NSWorkspace.screensDidSleepNotification] {
            workspaceTokens.append(workspace.addObserver(forName: name, object: nil, queue: .main, using: receive))
        }
        // These distributed names are additional revocation-only signals.
        // Availability is not an authentication guarantee; no wake/unlock event
        // can create or revive permission. Each action still uses fresh LA.
        for name in ["com.apple.screenIsLocked", "com.apple.screensaver.didstart"] {
            let key = Notification.Name(name)
            if let distributed { distributedTokens.append(distributed.addObserver(forName: key, object: nil, queue: .main, using: receive)) }
            else { distributedTokens.append(DistributedNotificationCenter.default().addObserver(forName: key, object: nil, queue: .main, using: receive)) }
        }
    }
    deinit {
        for token in workspaceTokens { workspace.removeObserver(token) }
        for token in distributedTokens {
            if let injectedDistributed { injectedDistributed.removeObserver(token) }
            else { DistributedNotificationCenter.default().removeObserver(token) }
        }
    }
}

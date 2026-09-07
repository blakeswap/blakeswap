# Deadline alerts and shutdown protection

Blakeswap monitors all saved wallets on the active network while the native app
owns its helper. The monitoring panel and Quit check share the daemon's typed
obligation summary. Selecting another wallet or a different history page does
not hide swaps, signed sends, open orders, enabled automatic offers, accepted
local rescue jobs, or restored obligations awaiting reconciliation.

The summary distinguishes local knowledge from chain availability. An encrypted
local snapshot can establish that a wallet is empty or positively settled even
when its endpoints cannot connect. An unreadable vault, opening placeholder,
stale live snapshot or command still publishing its result is explicitly
unknown. Unknown does not claim a funded trade exists; it means the app cannot
complete the all-wallet check. Ordinary endpoint outages do not reverse saved
positive settlement outcomes. A known reorg hold reopens monitoring despite the
old displayed outcome, and remains visible until positive evidence resolves it.

## Timing and action information

Regtest deadlines show the observed height, threshold and remaining blocks.
Public-network deadlines use the chain's median time past (MTP), its observation
time and remaining MTP seconds. Calendar dates are estimates: wall time cannot
make a consensus lock eligible. Timestamp refunds/rescues require MTP strictly
after the lock; the reveal cutoff is exclusive. The peer-chain reveal safety
margin is shown separately because the two chains can advance independently.

An unavailable, old or inconsistent clock is labelled uncertain, rather than
counting down from the laptop clock. Reveal alerts require current observations
from both chains and compatible public chain clocks. A known target-chain
refund/rescue deadline can still alert during an unrelated peer outage; the
alert describes timing, not permission to broadcast. The daemon continues to
apply its existing funding, revelation, refund and publication checks.

A taker's first secret revelation still requires this app. An external tower
cannot reveal that secret first, even when its exact rescue receipts are armed.
Restored private trades cannot resume first revelation; their recovery rules
continue to govern settlement. Failed publication and signed pending sends
remain monitoring obligations after an ambiguous response.

## Private notifications

Settings provides independent preferences for transitions, approaching
contract deadlines, failed/reopened work and confirmed settlements. macOS
notification permission is requested using **Enable macOS notifications**.
Denied permission leaves the in-app panel and Quit protection working.

Lock-screen messages contain no wallet names, amounts, addresses, secret
material or transaction identifiers. An opaque notification identifier resolves
through a private local journal to the saved wallet and relevant order, swap,
send or monitoring detail. Switching networks remains an explicit Settings
action; a notification cannot silently change the network of an active wallet.

The private `notifications/journal.json` file records preferences and opaque
consumed event identities before delivery. Identical state/deadline-band events
are not replayed after restart. Disabled or permission-denied events are
consumed rather than delivered in a later burst. Initial terminal history is a
baseline, not a batch of new success notifications. A reorg can notify that
monitoring reopened without replaying the previous success. Unchanged polling
does not rewrite the journal.

Notifications are submitted immediately from eligible observations. No future
local timer is presented as a substitute for chain monitoring. Sleep/wake and
session reconnection request fresh worker checks; the interruption banner stays
visible until the affected observations are current. One unavailable wallet
cannot silence a fresh deadline in a different wallet.

## Closing the app

Quit and closing the last window check every saved wallet. When work or unknown
local state remains, **Stay open** preserves the window and helper; **Quit**
explicitly stops them. Empty/settled installations close normally. The prompt
includes local tower jobs and first-revelation needs regardless of the selected
wallet. Its counts summarize the detailed monitoring panel.

Shutdown cancels the owned helper, waits for cleanup and bounds the wait. A
helper that does not exit after the grace interval is terminated forcibly; its
owned private runtime is removed after process exit. The native process will
not restart the helper once shutdown begins. External consensus nodes are not
stopped, and no background service or orphan monitoring process is installed.

Tests use deterministic observation times and an injected notification provider.
Native lifecycle tests use the freshly built real helper with private test
profiles, and verify both Stay open and Quit, last-window interception,
credential/socket/runtime cleanup and bounded shutdown. These lifecycle tests do
not claim to be pixel-level notification permission UI automation or a live
public-network deadline calibration.

The notification journal has a 16 MiB file limit and a 100,000-entry limit for
each identity/state/route set. Size is checked before decoding. Obsolete route
metadata is pruned, while exact consumed hashes remain. Reaching a limit pauses
notification delivery with an in-app Settings error; all-wallet monitoring and
Quit protection continue. The app does not silently discard history or replay
old success. Retain the journal if investigating a capacity error. Normal
thousands-of-events history fits below these ceilings.

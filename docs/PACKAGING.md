# Native macOS application and DMG

## Contents and lifecycle

Blakeswap targets macOS 15 or later using SwiftUI, SwiftProtobuf, and gRPC Swift 2.
The app bundle contains one native UI executable and the Go `blakeswap` helper,
plus documentation and dependency license/privacy resources.

Opening the app requests macOS owner authentication, then launches
`Contents/Resources/blakeswap desktop --credential-mode native --data-dir …
--parent-pid …`. The helper holds an exclusive lock on that data directory and
owns the wallet engines, API listeners, and runtime credential files. Settings
supports creating independent wallets and editing their display names. All saved
wallets are selectable and run on the active network. New installations create or
restore their first wallet in onboarding; legacy Alice/Bob vaults retain their original encrypted master seeds.
The isolated regtest demonstration explicitly prepares Alice and Bob.

The opening screen waits for the helper to publish its private runtime endpoint
before requesting wallet status. A briefly absent `runtime.json` during launch
is normal and does not display a file error. The wait is cancellable and bounded
to 15 seconds; helper exits, invalid/private-file checks, and startup timeouts
remain visible as connection errors.

There is no pause control. An unexpectedly exited helper needs a new initial unlock before it reopens;
**Unlock wallet service** explicitly retries a cancelled or denied prompt.
Old persisted pause flags are cleared on reopen.
Watchtower service runs alongside trading, with public listing off by default.

Quitting, including closing the last window, sends SIGTERM to the owned helper
and waits for it to release its vaults and API endpoints. The helper independently
checks its parent PID every 300 ms and cancels on parent death, so force-killing
the GUI also stops its daemon.

Closing the app stops swap progress and observations. Chain deadlines continue.
An armed external watchtower can execute its already-authorized rescues while the
app is closed. It cannot accept a new offer, fund a new leg, or perform the first
secret revelation for an offline taker. Sleep and network outages also interrupt
progress. Reopening resumes the persisted state.

Normal application data is `~/Library/Application Support/Blakeswap`:

- `settings.json`: all three environments, revision and onboarding stage, mode 0600.
- `runtime.json`: current socket/HTTP endpoints and bearer credentials, mode 0600.
- `desktop.lock`, `desktop.log`: process ownership and error log.
- `wallets/alice/master.db`: encrypted master seed shared across that profile's networks.
- `installation.json`: private local installation ID, not a portable credential.
- `wallets/alice/credential.json`: verified migration phase and exact Keychain account reference; no password.
- `wallets/<id>/profile.json`: public identity marker for interrupted profile publication.
- `vault.password` exists only in explicit file mode or during an incomplete legacy migration.
- `wallets/alice/<network>/state.db`: isolated network-specific swap state.
- Additional `wallets/<id>/` directories have their own master and network state.

`--data-dir /absolute/path` selects an isolated desktop installation for testing.
Do not point two concurrently running copies at the same wallet data.

## First launch and reset

First launch offers a new 24-word wallet, restoration from a BIP39 phrase, or
restoration from an encrypted wallet state backup. New and phrase-restored wallets
require confirmation of three recovery words. The phrase restores keys for both
chains; an encrypted state backup also preserves pending swaps and prepared
rescue transactions. Setup can export a backup protected by a password you choose
(at least 16 characters). Keep the password separately. Older state backups use
the original installation's `vault.password` contents.

Setup then presents network, chain endpoints, and Nostr relays, with an optional
connection check. No wallet engine connects or trades until setup completes.
Quitting during setup resumes the same prepared wallet on reopening. Existing
settings from earlier releases are treated as already configured.

For development, quit the app and run:

```sh
make reset-local-data
# Or reset only an isolated installation:
make reset-local-data APP_DATA_DIR="/absolute/path/to/test-data"
```

The command moves the active data directory to a dated sibling archive and
prints its location. It refuses a running app or an unrelated directory. It
does not delete the archive; the next launch creates fresh settings and displays
onboarding. To resume the exact archived installation, quit the app and launch
with `--data-dir /absolute/archive/path`. A native archive also needs its original
Keychain item; use a portable backup when moving to another Mac. Old file-mode
archives may contain wallet passwords. Keep all archives private. Resetting local storage does not cancel any
existing on-chain obligations.

## Credentials and authentication

The native app owns non-synchronizing Keychain items, scoped by installation,
profile and a fresh record ID. Their access list names the signed app identity;
the verified bundled helper receives separately owned bytes through inherited
anonymous pipes. Those pipes carry no stdout logs and have no public approval
endpoint. The runtime manifest contains endpoint tokens and process/session
identity, never a vault password, Keychain item data or an approved grant.

Existing file installations migrate under the installation lock. A private
journal records item creation, readback, verification of the existing master and
all present network vaults, activation, and removal of the old password file.
Interrupted steps resume the same record without replacing it or generating a
new identity. A denied, locked, missing or mismatched active item is an explicit
error; desktop startup never selects an old file as a fallback. File removal
cannot erase historic filesystem snapshots. Failed additional-wallet setup stays
in private staging; a verified published profile survives later settings failure
and completes publication after an authenticated restart.

Initial unlock and every new sensitive request use macOS
`deviceOwnerAuthentication`, with the OS-managed Mac password fallback when
Touch ID is unavailable. The app never collects the Mac login password. Cancelling
or denying a prompt leaves the action unauthorized. Approvals bind the exact
wallet/key, network, helper session, reviewed request and current revision; they
are one-use and expire. Wallet creation/import, recovery disclosure/export,
withdrawal/trade authorization and spending/automation/settings edits all require
this boundary, including compatibility API routes and stop/disable actions.

Screen sleep, system sleep and user-session departure revoke pending permission.
Screen-lock and screensaver distributed events provide additional revocation
signals whose availability can vary by macOS version; returning/unlocking never
approves or resurrects a request. Each new sensitive action still needs fresh OS
authentication. The helper keeps already-loaded keys and advances accepted trades,
raw signed retries and already-reviewed bounded automatic policies while the app
remains open. Closing the private consent connection retires new approvals, rejects
in-flight sensitive replies, and clears recovery displays without stopping these
obligations. A disconnected owner must reopen the app to establish
a new private session; normal Quit still performs the all-wallet shutdown check.
This is a hot-wallet session, not hardware-wallet protection or key eviction on
screen lock. See [Risks](RISKS.md).

The private pipe accepts frames up to 128 KiB, at most 32 pending calls and 32
incoming handlers. Outgoing work has a separate 32-frame / 4,194,432-byte bound
(including framing), and a 45-second write deadline. Cancelling an unsent frame
discards its owned bytes; cancelling a partially written frame closes the
connection so it cannot be misread as a different message. No stalled reader can
grow an unbounded queue of credential or consent replies.

A portable backup contains durable wallet state encrypted under its chosen backup
password. It contains no original Keychain reference or ephemeral permission.
Restoring on another installation creates a fresh local Keychain account and
retains recovery, imported-policy and uncertain-budget holds. Authentication does
not clear those holds.

For deliberate headless operation, use `blakeswap desktop --credential-mode file
--data-dir /absolute/private/path`, or set `"credential_mode": "file"` in a standalone
`daemon --config` file. File mode gives the operator's authenticated API clients
spending authority without native prompts and retains the private password file.
The development launcher explicitly selects it. There is no silent desktop
fallback and no public endpoint that approves a native challenge.

## Build and install

Use an Apple silicon or Intel Mac with compatible Swift 6.1+ tooling, Python 3,
and Go toolchain download support. Each build contains a native UI and Go helper
for its host architecture. The [GitHub release workflow](../.github/workflows/release.yml)
is configured to build separate arm64 and x86_64 DMGs.

```sh
sh scripts/build-mac.sh
sh scripts/build-dmg.sh
```

The first script builds/signs `bin/Blakeswap.app`. The second creates and verifies
`bin/Blakeswap-0.2.0-arm64.dmg` (or `x86_64` on Intel), with the app and an Applications shortcut. Open the
DMG, drag Blakeswap into Applications, eject the image, and open the installed app.
No repository checkout or separately installed Go runtime is needed to run it.
Native dependencies are linked into the executable. Building does require the
pinned package downloads; node downloads are not part of packaging.

The source app icon is `macos/Assets/AppIcon.png`. The build uses macOS `sips`
and `iconutil` to package its 16, 32, 128, 256, and 512 point representations at
1× and 2× resolution, preserving transparency. `AppIcon.icns` is included in
the app's Resources directory and declared in `Info.plist` before code signing.
The generated artwork and its prompt are kept together in `macos/Assets/`.

Without signing credentials, builds are **ad-hoc signed local development
artifacts**, not notarized public releases. Gatekeeper may reject a downloaded
ad-hoc build. For a distributable signed/notarized build, configure your own
Developer ID Application identity and an existing notarytool Keychain profile:

```sh
BLAKESWAP_SIGN_IDENTITY='Developer ID Application: …' \
BLAKESWAP_NOTARY_PROFILE='your-existing-profile' sh scripts/build-dmg.sh
```

The script signs nested executables with hardened runtime, signs the app and DMG,
submits to Apple's notary service, waits for acceptance, and staples the DMG. It
does not create signing identities or upload credentials. A failed notarization
fails the build. `codesign --verify --deep --strict` and `hdiutil verify` run before
success is reported. App installation in the build directory is an atomic move,
so rebuilding cannot overwrite the executable vnode of a running copy.

## GitHub release downloads

Push a version tag such as `v0.3.0`, or publish a GitHub release for an existing
version tag. The macOS packages workflow builds the tagged source on native
Apple silicon (`macos-26`) and Intel (`macos-26-intel`) runners, verifies both
executables' architectures, and runs packaging/launcher tests plus `swift test`
with the built helper. That enables native startup and onboarding tests. The
external regtest gRPC trade skips without `BLAKESWAP_SWIFT_TEST_ROOT`; this workflow
does not set up two-chain nodes. It uploads these assets only after both jobs pass:

- `Blakeswap-0.3.0-arm64.dmg` (Apple silicon)
- `Blakeswap-0.3.0-x86_64.dmg` (Intel)
- A SHA-256 checksum file for each DMG

If the tag has no release, the workflow creates one; otherwise it attaches assets
to the existing release without replacing its notes. Prerelease tags such as
`v0.3.0-rc.1` produce prereleases. Both DMG filenames and app metadata derive from
the tag. DMG builds run only for version tags and published releases. Pull requests
and main-branch pushes keep the Go checks without building a Mac installer.
For local builds, `BLAKESWAP_VERSION=v0.3.0 sh scripts/build-dmg.sh` overrides the
version (otherwise an exact version tag or `0.2.0` is used).

Hosted builds currently use ad-hoc signing. No Developer ID certificate or Apple
notarization credentials are configured in this workflow; downloaded DMGs may
be blocked by Gatekeeper. The existing local signing/notarization options above
remain available. A workflow definition establishes intended build/test steps,
not that a particular tag built, shipped, or received independent verification.
Check that tag’s workflow results and release assets before treating it as built.

## Explicit external regtest demonstration

The following developer harness downloads/starts separate full nodes and a local
relay. None becomes part of the app or DMG. It writes only an isolated demo data
directory, and configures the app to connect to those endpoints:

```sh
sh scripts/build-mac.sh
python3 scripts/desktop-demo.py prepare
open bin/Blakeswap.app --args --data-dir "$PWD/.local/desktop-demo"
python3 scripts/desktop-demo.py status
```

Use the native UI to create and take trades; each new authorization requests
macOS authentication. The CLI trade command cannot approve native consent. For an
unattended demo, run a separate explicit file-mode helper on the prepared isolated
root and invoke `python3 scripts/desktop-demo.py trade`; do not simultaneously open
the native app on that root. This command funds only regtest addresses and writes
public transaction evidence to `.local/desktop-demo/successful-trade.json`. After quitting the app, explicitly stop
fixtures with `python3 scripts/desktop-demo.py stop-relay` and
`python3 scripts/local.py stop-nodes`.

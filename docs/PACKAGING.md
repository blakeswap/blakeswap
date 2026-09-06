# Native macOS application and DMG

## Contents and lifecycle

Blakeswap targets macOS 15 or later using SwiftUI, SwiftProtobuf, and gRPC Swift 2.
The app bundle contains one native UI executable and the Go `blakeswap` helper,
plus documentation and dependency license/privacy resources.

Opening the app launches `Contents/Resources/blakeswap desktop --data-dir …
--parent-pid …`. The helper holds an exclusive lock on that data directory and
owns the wallet engines, API listeners, and runtime credential files. Settings
supports creating independent wallets and editing their display names. All saved
wallets are selectable and run on every network. New installations create or
restore their first wallet in onboarding; legacy Alice/Bob vaults retain their original encrypted master seeds.
The isolated regtest demonstration explicitly prepares Alice and Bob.

The opening screen waits for the helper to publish its private runtime endpoint
before requesting wallet status. A briefly absent `runtime.json` during launch
is normal and does not display a file error. The wait is cancellable and bounded
to 15 seconds; helper exits, invalid/private-file checks, and startup timeouts
remain visible as connection errors.

There is no pause control. The client restarts an unexpectedly exited helper while
the app remains open, and old persisted pause flags are cleared on reopen.
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
- `wallets/alice/vault.password`: random local vault password, mode 0600.
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
with `--data-dir /absolute/archive/path`. Archives contain wallet passwords and
pending state, so keep them private. Resetting local storage does not cancel any
existing on-chain obligations.

## Build and install

Use an Apple Silicon Mac with Xcode/Swift 6.3 or compatible Swift 6 tooling,
Python 3, and Go toolchain download support. Current scripts produce a host-arch
build; a universal Intel/Apple Silicon artifact is not implemented.

```sh
sh scripts/build-mac.sh
sh scripts/build-dmg.sh
```

The first script builds/signs `bin/Blakeswap.app`. The second creates and verifies
`bin/Blakeswap-0.2.0-arm64.dmg`, with the app and an Applications shortcut. Open the
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

## Explicit external regtest demonstration

The following developer harness downloads/starts separate full nodes and a local
relay. None becomes part of the app or DMG. It writes only an isolated demo data
directory, and configures the app to connect to those endpoints:

```sh
sh scripts/build-mac.sh
python3 scripts/desktop-demo.py prepare
open bin/Blakeswap.app --args --data-dir "$PWD/.local/desktop-demo"
python3 scripts/desktop-demo.py trade
python3 scripts/desktop-demo.py status
```

The trade command funds only regtest addresses, sends typed gRPC requests to the
app-owned daemon, mines external test-node blocks, and writes public transaction
evidence to `.local/desktop-demo/successful-trade.json`. The UI can instead create,
take, and mine the same trade manually. After quitting the app, explicitly stop
fixtures with `python3 scripts/desktop-demo.py stop-relay` and
`python3 scripts/local.py stop-nodes`.

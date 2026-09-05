#!/usr/bin/env python3
"""Check the desktop distribution includes exactly the UI and wallet daemon."""
import pathlib, plistlib, struct, sys
app = pathlib.Path(sys.argv[1])
with (app / 'Contents/Info.plist').open('rb') as file:
    info = plistlib.load(file)
if info.get('CFBundleIconFile') != 'AppIcon':
    raise SystemExit('Missing app icon declaration')
icon = app / 'Contents/Resources/AppIcon.icns'
with icon.open('rb') as file:
    header = file.read(8)
if len(header) != 8 or header[:4] != b'icns' or struct.unpack('>I', header[4:])[0] != icon.stat().st_size:
    raise SystemExit('Invalid app icon resource')
found = []
for path in app.rglob('*'):
    if path.name in ('bitcoind', 'bitcoin-cli', 'electrs', 'fulcrum', 'electrumx'):
        raise SystemExit(f'Unexpected chain executable in app: {path}')
    if path.is_file():
        with path.open('rb') as file: magic = file.read(4)
        if magic in (bytes.fromhex('cffaedfe'), bytes.fromhex('feedfacf'), bytes.fromhex('cafebabe')):
            found.append(str(path.relative_to(app)))
expected = ['Contents/MacOS/Blakeswap', 'Contents/Resources/blakeswap']
if sorted(found) != expected: raise SystemExit(f'Unexpected executables: {found}')
print('Verified bundle: app icon, native UI and Go daemon')

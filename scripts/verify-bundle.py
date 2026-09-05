#!/usr/bin/env python3
"""Check the desktop distribution includes exactly the UI and wallet daemon."""
import pathlib, sys
app = pathlib.Path(sys.argv[1])
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
print('Verified bundle: native UI and Go daemon; no chain nodes or indexers')

#!/usr/bin/env python3
"""Copy distribution notices from the exact resolved dependencies, without network access."""
import json, pathlib, shutil, subprocess, sys
ROOT = pathlib.Path(__file__).resolve().parents[1]
DEST = pathlib.Path(sys.argv[1])
def notices(source, name):
    source = pathlib.Path(source)
    for path in source.rglob('*'):
        if not path.is_file() or '.git' in path.parts: continue
        if not path.name.upper().startswith(('LICENSE', 'LICENCE', 'COPYING', 'NOTICE')): continue
        relative = path.relative_to(source)
        target = DEST / name / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, target)
for checkout in (ROOT / '.cache/swift-build/checkouts').iterdir():
    if checkout.is_dir(): notices(checkout, 'swift/' + checkout.name)
output = subprocess.check_output(['sh','scripts/go.sh','list','-m','-json','all'], cwd=ROOT, text=True)
decoder=json.JSONDecoder()
while output.strip():
    output=output.lstrip();module,end=decoder.raw_decode(output);output=output[end:]
    if module.get('Main'): continue
    if module.get('Dir'): notices(module['Dir'], 'go/'+module['Path']+'@'+module.get('Version',''))

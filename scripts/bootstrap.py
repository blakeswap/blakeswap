#!/usr/bin/env python3
"""Download pinned upstream nodes into a project-local cache; verify SHA256."""
import argparse, hashlib, pathlib, platform, tarfile, tempfile, time, urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
CACHE = ROOT / ".cache" / "nodes"
RELEASES = {
    "btc": ("29.1", "https://bitcoincore.org/bin/bitcoin-core-29.1/"),
    "blake": ("29.4.1.knots20260508", "https://bitcoinknots.org/files/29.x/29.4.1.knots20260508/"),
}

def fetch(url, path):
    if not path.exists():
        print("Downloading", url, flush=True)
        with urllib.request.urlopen(url, timeout=120) as r:
            path.write_bytes(r.read())

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("chain", nargs="?", choices=list(RELEASES))
    args = parser.parse_args()
    target = {("Darwin", "arm64"): "arm64-apple-darwin", ("Darwin", "x86_64"): "x86_64-apple-darwin", ("Linux", "x86_64"): "x86_64-linux-gnu", ("Linux", "aarch64"): "aarch64-linux-gnu"}[(platform.system(), platform.machine())]
    for name, (version, base) in RELEASES.items():
        if args.chain and args.chain != name: continue
        dest = CACHE / name
        dest.mkdir(parents=True, exist_ok=True)
        sums = dest / "SHA256SUMS"
        fetch(base + "SHA256SUMS", sums)
        filename = f"bitcoin-{version}-{target}.tar.gz"
        expected = next(line.split()[0] for line in sums.read_text().splitlines() if line.split()[-1].lstrip("*") == filename)
        archive = dest / filename
        fetch(base + filename, archive)
        actual = hashlib.sha256(archive.read_bytes()).hexdigest()
        if actual != expected:
            raise RuntimeError(f"Checksum mismatch: {archive}")
        installed = dest / f"bitcoin-{version}"
        stamp = installed / ".archive-sha256"
        # Never overwrite a running signed Mach-O executable in place: macOS
        # can kill that process and reject the modified vnode's cached signature.
        if not stamp.exists() or stamp.read_text().strip() != actual:
            with tempfile.TemporaryDirectory(prefix="install-", dir=dest) as stage:
                with tarfile.open(archive) as tar:
                    tar.extractall(stage, filter="data")
                fresh = pathlib.Path(stage) / f"bitcoin-{version}"
                (fresh / ".archive-sha256").write_text(actual + "\n")
                if installed.exists():
                    installed.rename(dest / f"previous-{version}-{time.time_ns()}")
                fresh.rename(installed)
        print(name, actual, installed / "bin" / "bitcoind", flush=True)

if __name__ == "__main__": main()

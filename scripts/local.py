#!/usr/bin/env python3
"""Isolated real-chain regtest lifecycle. No mainnet or external P2P access."""
import argparse, base64, json, os, pathlib, subprocess, time, urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
NODES = {"btc": ("29.1", int(os.environ.get("BLAKESWAP_BTC_RPC_PORT", "19443"))), "blake": ("29.4.1.knots20260508", int(os.environ.get("BLAKESWAP_BLAKE_RPC_PORT", "29443")))}

def rpc(chain, method, *params, wallet=False):
    _, port = NODES[chain]
    cookie = (ROOT / ".local" / chain / "regtest" / ".cookie").read_text().strip()
    url = f"http://127.0.0.1:{port}" + ("/wallet/faucet" if wallet else "")
    req = urllib.request.Request(url, json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": list(params)}).encode(),
        {"Authorization": "Basic " + base64.b64encode(cookie.encode()).decode(), "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r: reply = json.load(r)
    except urllib.error.HTTPError as e: reply = json.load(e)
    if reply.get("error"): raise RuntimeError(reply["error"])
    return reply["result"]

def start(chain):
    version, port = NODES[chain]
    data = ROOT / ".local" / chain
    data.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        rpc(chain, "getblockchaininfo")
    except (OSError, RuntimeError):
        exe = ROOT / ".cache" / "nodes" / chain / f"bitcoin-{version}" / "bin" / "bitcoind"
        args = [str(exe), f"-datadir={data}", "-regtest", "-server", "-daemonwait", "-listen=0", "-connect=0", "-dnsseed=0", "-discover=0", "-natpmp=0", "-txindex=1", "-fallbackfee=0.00002", "-rpcbind=127.0.0.1", "-rpcallowip=127.0.0.1", f"-rpcport={port}"]
        if chain == "blake":
            args += ["-testactivationheight=blake2b@1"]
            if os.environ.get("BLAKESWAP_RDTS") == "1": args += ["-rdtsexpiry=4102444800"]
        subprocess.run(args, check=True)
    loaded = rpc(chain, "listwallets")
    if "faucet" not in loaded:
        existing = [w["name"] for w in rpc(chain, "listwalletdir")["wallets"]]
        rpc(chain, "loadwallet" if "faucet" in existing else "createwallet", "faucet")
    height = rpc(chain, "getblockcount")
    if height < 110:
        addr = rpc(chain, "getnewaddress", wallet=True)
        rpc(chain, "generatetoaddress", 110-height, addr)
    return rpc(chain, "getblockchaininfo")

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=["nodes", "stop-nodes", "status", "mine", "fund"])
    parser.add_argument("chain", nargs="?", choices=list(NODES))
    parser.add_argument("value", nargs="?")
    parser.add_argument("amount", nargs="?", default="1")
    a = parser.parse_args()
    for chain in ([a.chain] if a.chain else NODES):
        if a.action == "nodes":
            info = start(chain)
            print(chain, "height", info["blocks"], "tip", info["bestblockhash"])
            if chain == "blake":
                deployment = rpc(chain, "getdeploymentinfo")["blake2b"]
                assert deployment["active"], deployment
                print("Blake2b active:", deployment)
        elif a.action == "stop-nodes":
            try: print(chain, rpc(chain, "stop"))
            except (OSError, RuntimeError) as e: print(chain, str(e))
        elif a.action == "status": print(chain, json.dumps(rpc(chain, "getblockchaininfo"), indent=2))
        elif a.action == "mine":
            print(chain, rpc(chain, "generatetoaddress", int(a.value or "1"), rpc(chain, "getnewaddress", wallet=True)))
        elif a.action == "fund":
            if not a.chain or not a.value: parser.error("fund requires chain and address")
            print(rpc(chain, "sendtoaddress", a.value, float(a.amount), wallet=True))
            rpc(chain, "generatetoaddress", 1, rpc(chain, "getnewaddress", wallet=True))

if __name__ == "__main__": main()

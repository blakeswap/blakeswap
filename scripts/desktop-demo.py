#!/usr/bin/env python3
"""Developer fixture for the desktop app.

prepare starts regtest services and writes isolated Settings; it never
installs or changes the normal desktop wallet. Launch the app with the printed
--data-dir. trade uses the app-owned daemon's gRPC API and real test coins.
"""
import argparse, json, os, pathlib, subprocess, time
import local

ROOT = pathlib.Path(__file__).resolve().parents[1]
DATA = pathlib.Path(os.environ.get("BLAKESWAP_DESKTOP_DATA_DIR", str(ROOT / ".local/desktop-demo"))).resolve()
EXE = ROOT / "bin/Blakeswap.app/Contents/Resources/blakeswap"
RELAY_PORT = int(os.environ.get("BLAKESWAP_DEMO_RELAY_PORT", "17447"))

def call(profile, method, params=None):
    endpoints = json.loads((DATA / "runtime.json").read_text())
    raw = subprocess.check_output([str(EXE), "call", "--socket", endpoints[profile]["socket"], "--method", method, "--params", json.dumps(params or {})], text=True)
    return json.loads(raw)

def prepare():
    subprocess.run(["python3", "scripts/bootstrap.py"], cwd=ROOT, check=True)
    for chain in local.NODES: local.start(chain)
    DATA.mkdir(parents=True, exist_ok=True, mode=0o700)
    if (DATA / "runtime.json").exists(): raise RuntimeError("Quit the demo app before preparing its settings")
    if not (DATA / "settings.json").exists():
        with (DATA / "setup.log").open("ab") as log:
            helper = subprocess.Popen([str(EXE), "desktop", "--data-dir", str(DATA)], stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if (DATA / "settings.json").exists(): break
                    if helper.poll() is not None: raise RuntimeError("Desktop setup failed; inspect setup.log")
                    time.sleep(.1)
                else: raise RuntimeError("Settings initialization timed out")
            finally:
                helper.terminate(); helper.wait(timeout=30)
    settings = json.loads((DATA / "settings.json").read_text())
    settings["active_network"] = "regtest"
    settings["wallets"] = [{"id": "alice", "name": "Alice"}, {"id": "bob", "name": "Bob"}]
    for env in settings["environments"]:
        if env["network"] == "regtest":
            env["nodes"] = {chain: {"kind":"rpc", "url":f"http://127.0.0.1:{port}", "cookie":str(ROOT / ".local" / chain / "regtest/.cookie")} for chain, (_,port) in local.NODES.items()}
            env["relays"] = [f"ws://127.0.0.1:{RELAY_PORT}"]
            env["tower"] = {}
    (DATA / "settings.json").write_text(json.dumps(settings, indent=2)); (DATA / "settings.json").chmod(0o600)
    relay_log = (DATA / "relay.log").open("ab")
    pid_path = DATA / "relay.pid"
    running = False
    if pid_path.exists():
        try: os.kill(int(pid_path.read_text()), 0); running = True
        except ProcessLookupError: pass
    if not running:
        process = subprocess.Popen([str(EXE), "relay", "--db", str(DATA / "relay.db"), "--listen", f"127.0.0.1:{RELAY_PORT}"], stdout=relay_log, stderr=relay_log, start_new_session=True)
        pid_path.write_text(str(process.pid))
    relay_log.close()
    print(f'Open bin/Blakeswap.app with arguments --data-dir "{DATA}"')

def trade():
    for profile in ("alice","bob"):
        status = call(profile,"status")
        if status.get("network") != "regtest" or len(status.get("addresses",{})) != 2: raise RuntimeError("Demo wallets must be connected to regtest")
        for chain in local.NODES: call(profile,"regtest.faucet",{"chain":chain,"amount":100000000})
    call("alice","regtest.mine",{"blocks":2})
    offer = call("alice","offer.create",{"sell":"btc","sell_amount":1000000,"buy_amount":2000000})
    deadline=time.monotonic()+120
    while time.monotonic()<deadline:
        book=call("bob","status").get("orders",[])
        if any(o["id"]==offer["id"] for o in book): break
        time.sleep(.5)
    else: raise RuntimeError("Offer delivery timed out")
    swap=call("bob","swap.take",{"maker":offer["maker"],"id":offer["id"]})["id"]
    while time.monotonic()<deadline:
        states=[call(p,"status") for p in ("alice","bob")]
        legs=[next((s for s in state.get("swaps",[]) if s["id"]==swap),{}) for state in states]
        if all(s.get("stage")=="completed" for s in legs):
            report={"swap_id":swap,"maker":legs[0],"taker":legs[1]}
            (DATA / "successful-trade.json").write_text(json.dumps(report,indent=2)); print(json.dumps(report,indent=2)); return
        for chain in local.NODES:
            if local.rpc(chain,"getrawmempool"):
                local.rpc(chain,"generatetoaddress",2,local.rpc(chain,"getnewaddress",wallet=True))
        time.sleep(1)
    raise RuntimeError("Trade did not complete; inspect daemon status")

if __name__ == "__main__":
    parser=argparse.ArgumentParser(); parser.add_argument("action",choices=["prepare","status","trade","stop-relay"]);args=parser.parse_args()
    if args.action=="prepare":prepare()
    elif args.action=="trade":trade()
    elif args.action=="status":print(json.dumps({p:call(p,"status") for p in ("alice","bob")},indent=2))
    else:
        pid=DATA / "relay.pid"
        if pid.exists():
            process=int(pid.read_text())
            # Confirm this PID still belongs to our exact fixture before signaling it.
            command=subprocess.check_output(["ps","-p",str(process),"-o","command="],text=True).strip()
            if str(DATA / "relay.db") not in command: raise RuntimeError("Relay PID belongs to another process")
            os.kill(process,15); pid.unlink()

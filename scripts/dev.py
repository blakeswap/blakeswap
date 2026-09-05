#!/usr/bin/env python3
"""Build/start the local desktop stack and drive its real daemon API."""
import argparse, json, os, pathlib, secrets, signal, subprocess, sys, time
from local import ROOT, NODES, start

LOCAL = ROOT / ".local"
BIN = ROOT / "bin" / "blakeswap"

def call(name, method="status", params=None):
    result = subprocess.run([str(BIN), "call", "--socket", str(LOCAL/name/"daemon.sock"), "--method", method, "--params", json.dumps(params or {})], capture_output=True, text=True, timeout=50)
    if result.returncode: raise RuntimeError(result.stderr.strip())
    value = json.loads(result.stdout)
    if method == "status":
        for key in ("orders", "swaps", "tower_jobs"): value.setdefault(key, [])
        for key in ("balances", "heights", "addresses"): value.setdefault(key, {})
    return value

def wait_for(fn, timeout=30):
    deadline = time.monotonic()+timeout
    last = None
    while time.monotonic()<deadline:
        try:
            value = fn()
            if value: return value
        except (OSError, RuntimeError) as e: last=e
        time.sleep(0.25)
    raise RuntimeError(f"timed out: {last}")

def spawn(name, args):
    pidfile=LOCAL/(name+".pid")
    if pidfile.exists():
        pid=int(pidfile.read_text())
        process=subprocess.run(["ps","-p",str(pid),"-o","command="],capture_output=True,text=True)
        if process.returncode==0 and str(BIN) in process.stdout: return
    log=open(LOCAL/(name+".log"),"ab")
    child=subprocess.Popen([str(BIN)]+args,cwd=ROOT,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
    log.close();pidfile.write_text(str(child.pid))

def config(name, mode, tower):
    data=LOCAL/name;data.mkdir(parents=True,exist_ok=True,mode=0o700)
    password=data/"vault.password"
    if not password.exists():
        fd=os.open(password,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,"w") as f:f.write(secrets.token_hex(32))
    cfg={"name":name,"mode":mode,"data_dir":str(data),"password_file":str(password),"socket":str(data/"daemon.sock"),"relays":["ws://127.0.0.1:7447","ws://127.0.0.1:7448"],"nodes":{id:{"url":f"http://127.0.0.1:{port}","cookie":str(LOCAL/id/"regtest"/".cookie")} for id,(_,port) in NODES.items()},"tower":tower}
    path=data/"config.json";path.write_text(json.dumps(cfg,indent=2)+"\n");os.chmod(path,0o600)
    return path

def up():
    LOCAL.mkdir(exist_ok=True,mode=0o700)
    for id in NODES:start(id)
    staged=BIN.with_name("blakeswap.build")
    staged.parent.mkdir(exist_ok=True)
    subprocess.run(["sh","scripts/go.sh","build","-o",str(staged),"./cmd/blakeswap"],cwd=ROOT,check=True)
    staged.replace(BIN)
    for suffix,port in [("a",7447),("b",7448)]:spawn("relay-"+suffix,["relay","--db",str(LOCAL/("relay-"+suffix+".db")),"--listen",f"127.0.0.1:{port}"])
    tower_config=config("tower","tower",{"bps":50})
    spawn("tower",["daemon","--config",str(tower_config)])
    tower=wait_for(lambda:call("tower"))["tower"]
    for name in ["alice","bob"]:
        path=config(name,"trader",tower);spawn(name,["daemon","--config",str(path)]);state=wait_for(lambda:call(name))
        for chain in NODES:
            if state["balances"][chain]<100000000:call(name,"regtest.faucet",{"chain":chain,"amount":200000000})
    call("alice","regtest.mine",{"blocks":2})
    print("Running: two real regtest nodes, two Nostr relays, Alice, Bob, and tower.")
    for name in ["alice","bob","tower"]:
        state=call(name);print(name,state["pubkey"],state["addresses"])

def down():
    for name in ["alice","bob","tower","relay-a","relay-b"]:
        path=LOCAL/(name+".pid")
        if not path.exists():continue
        pid=int(path.read_text());process=subprocess.run(["ps","-p",str(pid),"-o","command="],capture_output=True,text=True)
        if process.returncode==0 and str(BIN) in process.stdout:
            os.kill(pid,signal.SIGTERM)
        path.unlink(missing_ok=True)
    print("Stopped application processes; regtest nodes remain available.")

def trade():
    offer=call("alice","offer.create",{"sell":"btc","sell_amount":1000000,"buy_amount":2000000,"tower_bps":50})
    wait_for(lambda:any(o["id"]==offer["id"] for o in call("bob")["orders"]))
    result=call("bob","swap.take",{"maker":offer["maker"],"id":offer["id"]});swapid=result["id"]
    print("Swap",swapid,flush=True);deadline=time.monotonic()+180;last=""
    while time.monotonic()<deadline:
        states={name:next((s for s in call(name)["swaps"] if s["id"]==swapid),{}) for name in ["alice","bob"]}
        summary=json.dumps({name:{"stage":s.get("stage"),"error":s.get("error")} for name,s in states.items()})
        if summary!=last:print(summary,flush=True);last=summary
        if all(s.get("stage")=="completed" for s in states.values()):
            report={"swap_id":swapid,"verified_at":time.time(),"states":states}
            path=LOCAL/"successful-trade.json";path.write_text(json.dumps(report,indent=2)+"\n")
            print("Successful atomic trade:",path);return
        # Mine only after there are transactions awaiting confirmations, never
        # consume timelock margins while a participant is negotiating offline.
        from local import rpc
        if any(rpc(id,"getrawmempool") for id in NODES):call("alice","regtest.mine",{"blocks":2})
        time.sleep(1)
    raise RuntimeError("trade did not complete; inspect .local/*.log and daemon status")

def main():
    parser=argparse.ArgumentParser();parser.add_argument("action",choices=["up","down","status","trade","call"]);parser.add_argument("name",nargs="?",default="alice");parser.add_argument("method",nargs="?",default="status");parser.add_argument("params",nargs="?",default="{}");a=parser.parse_args()
    if a.action=="up":up()
    elif a.action=="down":down()
    elif a.action=="trade":trade()
    elif a.action=="call":print(json.dumps(call(a.name,a.method,json.loads(a.params)),indent=2))
    else:
        for name in ["alice","bob","tower"]:print(name,json.dumps(call(name),indent=2))

if __name__=="__main__":main()

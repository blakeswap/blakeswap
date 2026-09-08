#!/usr/bin/env python3
"""Run one deterministic partition of the complete ordinary Go race suite."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


PACKAGES = ["./...", "fiatjaf.com/nostr/nip44"]
GO = ["sh", "scripts/go.sh", "test", "-race", "-count=1", "-p=1"]
NAME = re.compile(r"(?:Test|Example|Fuzz)\w*\Z")


def assigned(name, count):
    # Equal names in different packages must share a partition: -run applies
    # the same expression to every package. New tests cannot move old tests.
    return int.from_bytes(hashlib.sha256(name.encode()).digest()[:8], "big") % count


def listed_tests(output):
    tests = set()
    for line in output.splitlines():
        event = json.loads(line)
        name = event.get("Output", "").strip()
        if event.get("Action") == "output" and NAME.fullmatch(name):
            tests.add((event["Package"], name))
    if not tests:
        raise ValueError("Go discovery returned no tests, examples, or fuzz seeds")
    return tests


def selection(tests, index, count):
    if count < 1 or not 0 <= index < count:
        raise ValueError("shard index must be within the positive shard count")
    selected = {test for test in tests if assigned(test[1], count) == index}
    if not selected:
        raise ValueError("shard contains no tests")
    expression = "^(" + "|".join(re.escape(name) for name in sorted({name for _, name in selected})) + ")$"
    return selected, expression


def verify_execution(selected, events):
    ran, ended = {}, {}
    for event in events:
        name = event.get("Test", "")
        if not name or "/" in name:
            continue
        key = (event["Package"], name)
        if event["Action"] == "run":
            ran[key] = ran.get(key, 0) + 1
        elif event["Action"] in ("pass", "skip", "fail"):
            ended[key] = ended.get(key, 0) + 1
    if set(ran) != selected or set(ended) != selected or any(n != 1 for n in [*ran.values(), *ended.values()]):
        raise ValueError("executed tests do not exactly match the discovered shard")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("index", type=int)
    parser.add_argument("count", type=int)
    args = parser.parse_args()
    if args.count < 1 or not 0 <= args.index < args.count:
        parser.error("index must be within the positive shard count")
    os.chdir(Path(__file__).resolve().parent.parent)
    # Concurrent CI partitions are ordinary tests. Actual node and physical
    # resource gates have their own explicitly serialized runners.
    env = {key: value for key, value in os.environ.items() if not key.startswith("BLAKESWAP_")}
    discovery = subprocess.run(GO + ["-json", "-list", "."] + PACKAGES, env=env, text=True, stdout=subprocess.PIPE)
    if discovery.returncode:
        print(discovery.stdout, end="")
        return discovery.returncode
    tests = listed_tests(discovery.stdout)
    selected, expression = selection(tests, args.index, args.count)
    report = {"index": args.index, "count": args.count, "discovered": sorted(tests), "selected": sorted(selected)}
    directory = Path(".cache/ci")
    directory.mkdir(parents=True, exist_ok=True)
    (directory / f"go-shard-{args.index}.json").write_text(json.dumps(report, indent=2) + "\n")
    print(f"Shard {args.index + 1}/{args.count}: {len(selected)} of {len(tests)} discovered tests, examples, and fuzz seeds", flush=True)
    events = []
    with subprocess.Popen(GO + ["-json", "-run", expression] + PACKAGES, env=env, text=True, stdout=subprocess.PIPE) as process:
        for line in process.stdout:
            print(line, end="", flush=True)
            events.append(json.loads(line))
        result = process.wait()
    if result:
        return result
    verify_execution(selected, events)
    return 0


if __name__ == "__main__":
    sys.exit(main())

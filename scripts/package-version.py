#!/usr/bin/env python3
"""Validate release metadata and apply it to the app before signing."""
import argparse
import os
import plistlib
import re
import subprocess


def version():
    value = os.environ.get("BLAKESWAP_VERSION")
    if value is None:
        result = subprocess.run(["git", "describe", "--tags", "--exact-match", "HEAD"], capture_output=True, text=True)
        value = result.stdout.strip() if result.returncode == 0 else "0.2.0"
    value = value.removeprefix("v")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?", value):
        raise ValueError("Release version must be vMAJOR.MINOR.PATCH (optionally with a prerelease suffix)")
    return value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("plist", nargs="?")
    args = parser.parse_args()
    value = version()
    if args.plist:
        build = os.environ.get("BLAKESWAP_BUILD_NUMBER", value.split("-")[0])
        if not re.fullmatch(r"[0-9]+(?:\.[0-9]+){0,2}", build):
            raise ValueError("Build number must contain one to three numeric components")
        with open(args.plist, "rb") as file: info = plistlib.load(file)
        info["CFBundleShortVersionString"] = value.split("-")[0]
        info["CFBundleVersion"] = build
        info["BlakeswapReleaseVersion"] = value
        with open(args.plist, "wb") as file: plistlib.dump(info, file)
    else:
        print(value)


if __name__ == "__main__": main()

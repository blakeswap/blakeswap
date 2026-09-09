#!/usr/bin/env python3
"""Validate release metadata and apply it to the app before signing."""
import argparse
import os
import plistlib
import re


def version():
    value = os.environ.get("BLAKESWAP_VERSION", "1.0.0").removeprefix("v")
    if value != "1.0.0":
        raise ValueError("Blakeswap is unreleased; the application version must remain v1.0.0")
    return value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("plist", nargs="?")
    args = parser.parse_args()
    value = version()
    if args.plist:
        build = os.environ.get("BLAKESWAP_BUILD_NUMBER", value)
        if not re.fullmatch(r"[0-9]+(?:\.[0-9]+){0,2}", build):
            raise ValueError("Build number must contain one to three numeric components")
        with open(args.plist, "rb") as file: info = plistlib.load(file)
        info["CFBundleShortVersionString"] = value
        info["CFBundleVersion"] = build
        info["BlakeswapReleaseVersion"] = value
        with open(args.plist, "wb") as file: plistlib.dump(info, file)
    else:
        print(value)


if __name__ == "__main__": main()

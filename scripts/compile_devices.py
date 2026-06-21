#!/usr/bin/env python3
from __future__ import annotations

import argparse

from fleetlib import device_names, device_spec, ensure_secrets_link, run


def compile_device(name: str, firmware_version: str | None = None) -> None:
    spec = device_spec(name)
    ensure_secrets_link()
    cmd = ["esphome"]
    if firmware_version:
        cmd.extend(["-s", "firmware_version", firmware_version])
    cmd.extend(["compile", str(spec.config)])
    run(cmd)


def main() -> None:
    parser = argparse.ArgumentParser(description="Compile one or more devices with ESPHome.")
    parser.add_argument("devices", nargs="*", help="Device names to compile.")
    parser.add_argument("--all", action="store_true", help="Compile all devices.")
    parser.add_argument("--release-only", action="store_true", help="Compile only release devices when compiling all devices.")
    parser.add_argument("--firmware-version", help="Value to pass as the ESPHome firmware_version substitution.")
    args = parser.parse_args()

    names = device_names(release_only=args.release_only) if args.all or not args.devices else args.devices
    for name in names:
        compile_device(name, firmware_version=args.firmware_version)


if __name__ == "__main__":
    main()

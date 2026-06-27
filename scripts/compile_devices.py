#!/usr/bin/env python3
from __future__ import annotations

import argparse

from fleetlib import ROOT, device_names, device_spec, ensure_secrets_link, esphome_command, run


def compile_device(name: str, firmware_version: str | None = None, package_owner: str | None = None) -> None:
    spec = device_spec(name)
    ensure_secrets_link()
    cmd = esphome_command()
    if firmware_version:
        cmd.extend(["-s", "firmware_version", firmware_version])
    cmd.extend(["-s", "package_owner", package_owner or spec.package_owner])
    cmd.extend(["compile", str(spec.config.relative_to(ROOT))])
    run(cmd)


def main() -> None:
    parser = argparse.ArgumentParser(description="Compile one or more devices with ESPHome.")
    parser.add_argument("devices", nargs="*", help="Device names to compile.")
    parser.add_argument("--all", action="store_true", help="Compile all devices.")
    parser.add_argument("--release-only", action="store_true", help="Compile only release devices when compiling all devices.")
    parser.add_argument("--firmware-version", help="Value to pass as the ESPHome firmware_version substitution.")
    parser.add_argument("--package-owner", help="Value to pass as the ESPHome package_owner substitution.")
    args = parser.parse_args()

    names = device_names(release_only=args.release_only) if args.all or not args.devices else args.devices
    for name in names:
        compile_device(name, firmware_version=args.firmware_version, package_owner=args.package_owner)


if __name__ == "__main__":
    main()

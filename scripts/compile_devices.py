#!/usr/bin/env python3
from __future__ import annotations

import argparse

from fleetlib import device_names, device_spec, ensure_secrets_link, run


def compile_device(name: str) -> None:
    spec = device_spec(name)
    ensure_secrets_link()
    run(["esphome", "compile", str(spec.config)])


def main() -> None:
    parser = argparse.ArgumentParser(description="Compile one or more devices with ESPHome.")
    parser.add_argument("devices", nargs="*", help="Device names to compile.")
    parser.add_argument("--all", action="store_true", help="Compile all devices.")
    args = parser.parse_args()

    names = device_names(release_only=False) if args.all or not args.devices else args.devices
    for name in names:
        compile_device(name)


if __name__ == "__main__":
    main()

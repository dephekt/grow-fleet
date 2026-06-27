#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json

from firmware_inputs import firmware_impacted_devices, matrix_payload
from fleetlib import changed_paths


def main() -> None:
    parser = argparse.ArgumentParser(description="List devices impacted by changed files.")
    parser.add_argument("--base", help="Base revision for git diff.")
    parser.add_argument("--head", help="Head revision for git diff.")
    parser.add_argument("--path", action="append", default=[], help="Changed path to evaluate.")
    parser.add_argument("--json", action="store_true", help="Emit a JSON array.")
    parser.add_argument("--matrix", action="store_true", help='Emit a matrix payload: {"include":[...]}')
    args = parser.parse_args()

    paths = args.path or changed_paths(args.base, args.head)
    devices = firmware_impacted_devices(paths)

    if args.matrix:
        print(matrix_payload(devices))
        return
    if args.json:
        print(json.dumps(devices))
        return
    for device in devices:
        print(device)


if __name__ == "__main__":
    main()

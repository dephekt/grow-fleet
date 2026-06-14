#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json

from fleetlib import device_names


def main() -> None:
    parser = argparse.ArgumentParser(description="List fleet devices.")
    parser.add_argument("--release-only", action="store_true", help="List only release devices.")
    parser.add_argument("--json", action="store_true", help="Emit a JSON array.")
    parser.add_argument("--matrix", action="store_true", help='Emit a matrix payload: {"include":[...]}')
    args = parser.parse_args()

    names = device_names(release_only=args.release_only)

    if args.matrix:
        print(json.dumps({"include": [{"device": name} for name in names]}))
        return
    if args.json:
        print(json.dumps(names))
        return
    for name in names:
        print(name)


if __name__ == "__main__":
    main()

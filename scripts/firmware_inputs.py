#!/usr/bin/env python3
from __future__ import annotations

import json
from pathlib import Path
from typing import Iterable

from fleetlib import ROOT, device_names, iter_device_specs


SHARED_FIRMWARE_INPUTS = {
    "scripts/compile_devices.py",
    "scripts/fleetlib.py",
    "scripts/install_firmware_secrets.py",
}


def firmware_impacted_devices(paths: Iterable[str]) -> list[str]:
    path_set = {Path(path) for path in paths}
    if not path_set:
        return []

    path_strings = {str(path).replace("\\", "/") for path in path_set}
    if "fleet.yaml" in path_strings or path_strings.intersection(SHARED_FIRMWARE_INPUTS):
        return device_names(release_only=True)

    impacted: set[str] = set()
    for spec in iter_device_specs():
        watched = {str(Path(spec.config.relative_to(ROOT))).replace("\\", "/")}
        watched.update(str(asset.relative_to(ROOT)).replace("\\", "/") for asset in spec.assets)
        if path_strings.intersection(watched):
            impacted.add(spec.name)

    release_devices = set(device_names(release_only=True))
    return sorted(impacted.intersection(release_devices))


def matrix_payload(devices: Iterable[str]) -> str:
    return json.dumps({"include": [{"device": device} for device in devices]})

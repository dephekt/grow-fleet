#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path

from fleetlib import ROOT, capture, device_spec, esphome_version, fleet_component_dependency, sha256_file


def package_device(name: str, version: str, source_sha: str, dist_root: Path) -> Path:
    spec = device_spec(name)
    build_root = (
        ROOT
        / "devices/.esphome/build"
        / spec.esphome_name
        / ".pioenvs"
        / spec.esphome_name
    )
    ota_source = build_root / "firmware.ota.bin"
    factory_source = build_root / "firmware.factory.bin"
    if not ota_source.exists():
        raise FileNotFoundError(f"missing artifact: {ota_source}")
    if not factory_source.exists():
        raise FileNotFoundError(f"missing artifact: {factory_source}")

    device_dist = dist_root / name
    device_dist.mkdir(parents=True, exist_ok=True)

    ota_dest = device_dist / f"{name}.ota.bin"
    factory_dest = device_dist / f"{name}.factory.bin"
    shutil.copy2(ota_source, ota_dest)
    shutil.copy2(factory_source, factory_dest)

    artifact_filenames = [ota_dest.name, factory_dest.name]
    manifest = {
        "device": name,
        "package": spec.package,
        "version": version,
        "source_sha": source_sha,
        "component_dependency": fleet_component_dependency(),
        "esphome_version": esphome_version(),
        "artifact_filenames": artifact_filenames,
        "sha256": {
            ota_dest.name: sha256_file(ota_dest),
            factory_dest.name: sha256_file(factory_dest),
        },
    }

    manifest_path = device_dist / f"{name}.manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return manifest_path


def main() -> None:
    parser = argparse.ArgumentParser(description="Package a compiled firmware device.")
    parser.add_argument("device", help="Device name to package.")
    parser.add_argument("--version", required=True, help="Release or build version.")
    parser.add_argument(
        "--source-sha",
        default=None,
        help="Commit SHA to record in the manifest. Defaults to HEAD.",
    )
    parser.add_argument(
        "--dist-root",
        default="dist",
        help="Directory to write packaged artifacts into.",
    )
    args = parser.parse_args()

    source_sha = args.source_sha or capture(["git", "rev-parse", "HEAD"])
    manifest_path = package_device(args.device, args.version, source_sha, Path(args.dist_root))
    print(manifest_path)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Build, package, and publish Arduino (non-ESPHome) fleet firmware.

Parallel to compile_devices.py + package_device.py + publish_packages.py for
devices registered under ``arduino_devices:`` in fleet.yaml. Reuses fleetlib's
digest/version helpers and ``publish_packages.publish_device_oci`` so the
published OCI artifact is byte-compatible with the ESPHome devices' package
schema (``grow-firmware-package.v1``) and grow-app serves it unchanged.

Pipeline:  arduino-cli compile  ->  lzss compress  ->  bin2ota wrap
           ->  dist/<device>/<device>.ota.bin + <device>.manifest.json
           ->  oras push ghcr.io/dephekt/grow-fleet-<device>:<version>

The ``.ota`` is the LZSS-compressed image the UNO R4 WiFi OTAUpdate library
applies (proven manually; see arduino/README.md).
"""
from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
from datetime import UTC, datetime
from pathlib import Path

import yaml

import publish_packages
from fleetlib import (
    ROOT,
    assert_flashable_secrets,
    ensure_secrets_link,
    firmware_channel,
    md5_file,
    run,
    secret_values,
    sha256_file,
)

ARDUINO_DIR = ROOT / "arduino"
TOOLS_DIR = ARDUINO_DIR / "tools"
BUILD_ROOT = ARDUINO_DIR / ".build"
DIST_ROOT = ROOT / "dist"

# Site MQTT wiring baked into the firmware (matches the ESPHome fleet).
MQTT_USER = "edge-daniel-home"
MQTT_BROKER = "192.168.8.3"
MQTT_PORT = 1883

# arduino-cli FQBN -> the board key bin2ota/mkota understands (its magic number).
FQBN_OTA_BOARD = {
    "arduino:renesas_uno:unor4wifi": "UNOR4WIFI",
}


def generated_timestamp() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def arduino_device_spec(name: str) -> dict:
    data = yaml.safe_load((ROOT / "fleet.yaml").read_text(encoding="utf-8")) or {}
    devices = data.get("arduino_devices", {}) or {}
    if name not in devices:
        raise KeyError(f"unknown arduino device: {name} (add it under arduino_devices in fleet.yaml)")
    spec = dict(devices[name])
    for key in ("sketch", "fqbn", "node_id", "project_name", "package", "package_owner", "chip_family"):
        if key not in spec:
            raise ValueError(f"arduino device {name} missing required key: {key}")
    if spec["fqbn"] not in FQBN_OTA_BOARD:
        raise ValueError(f"no OTA board mapping for fqbn {spec['fqbn']} (extend FQBN_OTA_BOARD)")
    return spec


def arduino_cli() -> list[str]:
    return shlex.split(os.environ.get("ARDUINO_CLI", "arduino-cli"))


def _c_string(value: str) -> str:
    """Escape a value for embedding inside a C string literal. A secret containing
    a backslash or double-quote (all legal in WiFi/MQTT passwords and tokens) would
    otherwise produce a broken or silently-altered #define."""
    return value.replace("\\", "\\\\").replace('"', '\\"')


def write_secrets_header(sketch_dir: Path) -> None:
    ensure_secrets_link()
    s = secret_values()  # devices/secrets.yaml (real) or ci/secrets.yaml placeholders
    (sketch_dir / "arduino_secrets.h").write_text(
        f'#define SECRET_SSID "{_c_string(s["wifi_ssid"])}"\n'
        f'#define SECRET_PASS "{_c_string(s["wifi_password"])}"\n'
        f'#define SECRET_MQTT_BROKER "{MQTT_BROKER}"\n'
        f"#define SECRET_MQTT_PORT {MQTT_PORT}\n"
        f'#define SECRET_MQTT_USER "{MQTT_USER}"\n'
        f'#define SECRET_MQTT_PASS "{_c_string(s["mqtt_password"])}"\n'
        f'#define SECRET_FIRMWARE_UPDATE_TOKEN "{_c_string(s["firmware_update_token"])}"\n',
        encoding="utf-8",
    )


def write_version_header(sketch_dir: Path, version: str) -> None:
    (sketch_dir / "fw_version.h").write_text(f'#define FW_VERSION "{version}"\n', encoding="utf-8")


def ensure_lzss_so() -> None:
    if not (TOOLS_DIR / "lzss.so").exists():
        cc = os.environ.get("CC", "cc")
        run([cc, "-shared", "-fPIC", "-o", "lzss.so", "lzss.c"], cwd=TOOLS_DIR)


def compile_sketch(spec: dict, build_dir: Path) -> Path:
    if build_dir.exists():
        for f in build_dir.glob("*.ino.bin"):
            f.unlink()
    run(arduino_cli() + [
        "compile", "--fqbn", spec["fqbn"],
        "--output-dir", str(build_dir),
        str(ROOT / spec["sketch"]),
    ])
    bins = list(build_dir.glob("*.ino.bin"))
    if not bins:
        raise FileNotFoundError(f"no *.ino.bin produced in {build_dir}")
    return bins[0]


def make_ota(spec: dict, bin_path: Path, out_ota: Path) -> None:
    ensure_lzss_so()
    board = FQBN_OTA_BOARD[spec["fqbn"]]
    lzss = bin_path.with_suffix(".lzss")
    # lzss.py loads ./lzss.so relative to cwd -> run from TOOLS_DIR with absolute paths.
    run(["python3", "lzss.py", "--encode", str(bin_path.resolve()), str(lzss.resolve())], cwd=TOOLS_DIR)
    run(["python3", "mkota.py", board, str(lzss.resolve()), str(out_ota.resolve())], cwd=TOOLS_DIR)


def package(name: str, spec: dict, version: str, source_sha: str, build_profile: str) -> Path:
    channel = firmware_channel(version)  # validates edge-<UTC>-<sha> / vX.Y.Z
    flashable = build_profile == "site-private"

    sketch_dir = ROOT / spec["sketch"]
    build_dir = BUILD_ROOT / name
    build_dir.mkdir(parents=True, exist_ok=True)
    dist_dir = DIST_ROOT / name
    dist_dir.mkdir(parents=True, exist_ok=True)

    write_secrets_header(sketch_dir)
    write_version_header(sketch_dir, version)
    bin_path = compile_sketch(spec, build_dir)

    ota_dest = dist_dir / f"{name}.ota.bin"
    make_ota(spec, bin_path, ota_dest)

    names = [ota_dest.name]
    manifest = {
        "schema": "grow-firmware-package.v1",
        "channel": channel,
        "build_profile": build_profile,
        "flashable": flashable,
        "device": name,
        "node_id": spec["node_id"],
        "project_name": spec["project_name"],
        "package_owner": spec["package_owner"],
        "package": spec["package"],
        "version": version,
        "source_sha": source_sha,
        "chip_family": spec["chip_family"],
        "generated_at": generated_timestamp(),
        "artifact_filenames": names,
        "md5": {n: md5_file(dist_dir / n) for n in names},
        "sha256": {n: sha256_file(dist_dir / n) for n in names},
    }
    manifest_path = dist_dir / f"{name}.manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"packaged {name} {version} -> {manifest_path}")
    return manifest_path


def main() -> None:
    ap = argparse.ArgumentParser(description="Build/package/publish an Arduino fleet device.")
    ap.add_argument("device")
    ap.add_argument("--version", required=True, help="edge-<UTC>-<sha> or vX.Y.Z")
    ap.add_argument("--source-sha", default="")
    ap.add_argument("--build-profile", default="ci-placeholder", choices=["site-private", "ci-placeholder"])
    ap.add_argument("--require-flashable-secrets", action="store_true")
    ap.add_argument("--publish", action="store_true")
    ap.add_argument("--oci-registry", default=os.environ.get("OCI_REGISTRY", "ghcr.io"))
    ap.add_argument("--oci-owner", default=os.environ.get("OCI_OWNER", "dephekt"))
    ap.add_argument("--oci-package-prefix", default=os.environ.get("OCI_PACKAGE_PREFIX", "grow-fleet"))
    ap.add_argument("--oci-source-url", default=os.environ.get("OCI_SOURCE_URL", ""))
    args = ap.parse_args()

    spec = arduino_device_spec(args.device)
    if args.build_profile == "site-private" and args.require_flashable_secrets:
        ensure_secrets_link()
        assert_flashable_secrets()

    package(args.device, spec, args.version, args.source_sha, args.build_profile)

    if args.publish:
        publish_packages.publish_device_oci(
            DIST_ROOT,
            args.device,
            args.oci_registry,
            args.oci_owner,
            args.oci_package_prefix,
            args.oci_source_url or None,
        )


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import yaml


ROOT = Path(__file__).resolve().parents[1]
FLEET_PATH = ROOT / "fleet.yaml"


@dataclass(frozen=True)
class DeviceSpec:
    name: str
    config: Path
    esphome_name: str
    package: str
    release: bool
    assets: tuple[Path, ...]


def load_fleet() -> dict[str, Any]:
    if not FLEET_PATH.exists():
        raise FileNotFoundError(f"fleet definition not found: {FLEET_PATH}")
    with FLEET_PATH.open("r", encoding="utf-8") as handle:
        data = yaml.safe_load(handle)
    if not isinstance(data, dict):
        raise ValueError("fleet.yaml must contain a mapping")
    return data


def fleet_component_dependency() -> dict[str, Any]:
    data = load_fleet()
    component_dependency = data.get("component_dependency", {})
    if not isinstance(component_dependency, dict):
        raise ValueError("fleet.yaml component_dependency must be a mapping")
    return component_dependency


def iter_device_specs() -> list[DeviceSpec]:
    data = load_fleet()
    devices = data.get("devices", {})
    if not isinstance(devices, dict):
        raise ValueError("fleet.yaml devices must be a mapping")

    specs: list[DeviceSpec] = []
    for name, raw in devices.items():
        if not isinstance(raw, dict):
            raise ValueError(f"fleet device {name} must be a mapping")
        assets = tuple(ROOT / str(asset) for asset in raw.get("assets", []))
        specs.append(
            DeviceSpec(
                name=name,
                config=ROOT / str(raw["config"]),
                esphome_name=str(raw["esphome_name"]),
                package=str(raw["package"]),
                release=bool(raw.get("release", False)),
                assets=assets,
            )
        )
    return specs


def device_spec(name: str) -> DeviceSpec:
    for spec in iter_device_specs():
        if spec.name == name:
            return spec
    raise KeyError(f"unknown device: {name}")


def device_names(release_only: bool = False) -> list[str]:
    names = [spec.name for spec in iter_device_specs() if not release_only or spec.release]
    return sorted(names)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run(cmd: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None) -> None:
    subprocess.run(cmd, cwd=cwd or ROOT, env=env, check=True)


def capture(cmd: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None) -> str:
    completed = subprocess.run(
        cmd,
        cwd=cwd or ROOT,
        env=env,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    return completed.stdout.strip()


def esphome_version() -> str:
    candidates = [
        ["esphome", "version"],
        ["esphome", "--version"],
        ["python3", "-m", "esphome", "version"],
    ]
    last_error: Exception | None = None
    for candidate in candidates:
        try:
            return capture(candidate)
        except (subprocess.CalledProcessError, OSError) as exc:
            last_error = exc
    if last_error is not None:
        raise RuntimeError("unable to determine ESPHome version") from last_error
    raise RuntimeError("unable to determine ESPHome version")


def ensure_secrets_link() -> None:
    source = ROOT / "ci/secrets.yaml"
    target = ROOT / "devices/secrets.yaml"
    if target.exists() or not source.exists():
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source, target)


def changed_paths(base: str | None = None, head: str | None = None) -> list[str]:
    if base and head:
        cmd = ["git", "diff", "--name-only", f"{base}...{head}"]
    elif base:
        cmd = ["git", "diff", "--name-only", base]
    else:
        cmd = ["git", "diff", "--name-only", "HEAD~1"]
    output = capture(cmd)
    return [line for line in output.splitlines() if line.strip()]


def impacted_devices(paths: Iterable[str]) -> list[str]:
    path_set = {Path(path) for path in paths}
    if not path_set:
        return []

    if Path("fleet.yaml") in path_set:
        return device_names()

    specs = iter_device_specs()
    impacted: set[str] = set()
    path_strings = {str(path).replace("\\", "/") for path in path_set}

    for spec in specs:
        watched = {str(Path(spec.config.relative_to(ROOT))).replace("\\", "/")}
        watched.update(str(asset.relative_to(ROOT)).replace("\\", "/") for asset in spec.assets)
        if path_strings.intersection(watched):
            impacted.add(spec.name)

    if any(path.startswith("scripts/") or path.startswith(".forgejo/workflows/") for path in path_strings):
        impacted.update(spec.name for spec in specs)

    return sorted(impacted)


def matrix_payload(devices: Iterable[str]) -> str:
    return json.dumps({"include": [{"device": device} for device in devices]})

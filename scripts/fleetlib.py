#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import yaml


ROOT = Path(__file__).resolve().parents[1]
FLEET_PATH = ROOT / "fleet.yaml"
CI_SECRETS_PATH = ROOT / "ci/secrets.yaml"
DEVICE_SECRETS_PATH = ROOT / "devices/secrets.yaml"
REQUIRED_FLASHABLE_SECRET_KEYS = (
    "api_encryption_key",
    "fallback_hotspot_key",
    "firmware_update_token",
    "hydro_monitor_ota_key",
    "mqtt_password",
    "wifi_password",
    "wifi_ssid",
)


@dataclass(frozen=True)
class DeviceSpec:
    name: str
    config: Path
    esphome_name: str
    node_id: str
    project_name: str
    package_owner: str
    package: str
    chip_family: str
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
                node_id=str(raw.get("node_id", raw["esphome_name"])),
                project_name=str(raw.get("project_name", f"stackdrift.{raw['package']}")),
                package_owner=str(raw.get("package_owner", "stackdrift")),
                package=str(raw["package"]),
                chip_family=str(raw["chip_family"]),
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


def md5_file(path: Path) -> str:
    digest = hashlib.md5(usedforsecurity=False)
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def firmware_build_dir(spec: DeviceSpec) -> Path:
    return ROOT / "devices/.esphome/build" / spec.esphome_name / ".pioenvs" / spec.esphome_name


def firmware_artifacts(name: str) -> dict[str, Path]:
    build_root = firmware_build_dir(device_spec(name))
    return {
        "ota": build_root / "firmware.ota.bin",
        "factory": build_root / "firmware.factory.bin",
    }


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


def ensure_secrets_link(source: Path | None = None) -> None:
    selected_source = source
    if selected_source is None:
        env_source = os.environ.get("FLEET_SECRETS_PATH")
        selected_source = Path(env_source) if env_source else CI_SECRETS_PATH

    target = DEVICE_SECRETS_PATH
    if target.exists() or not selected_source.exists():
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(selected_source, target)


def secret_values(path: Path = DEVICE_SECRETS_PATH) -> dict[str, str]:
    if not path.exists():
        raise FileNotFoundError(f"secrets file not found: {path}")
    with path.open("r", encoding="utf-8") as handle:
        data = yaml.safe_load(handle)
    if not isinstance(data, dict):
        raise ValueError(f"secrets file must contain a mapping: {path}")
    return {str(key): str(value) for key, value in data.items()}


def flashable_secret_problems(
    secrets_path: Path = DEVICE_SECRETS_PATH,
    placeholder_path: Path = CI_SECRETS_PATH,
) -> list[str]:
    secrets = secret_values(secrets_path)
    placeholders = secret_values(placeholder_path) if placeholder_path.exists() else {}
    problems: list[str] = []

    for key in REQUIRED_FLASHABLE_SECRET_KEYS:
        value = secrets.get(key)
        if value is None or value == "":
            problems.append(f"missing required secret: {key}")
            continue
        if placeholders.get(key) == value:
            problems.append(f"secret still has compile-only placeholder value: {key}")

    return problems


def assert_flashable_secrets(
    secrets_path: Path = DEVICE_SECRETS_PATH,
    placeholder_path: Path = CI_SECRETS_PATH,
) -> None:
    problems = flashable_secret_problems(secrets_path, placeholder_path)
    if problems:
        raise ValueError("firmware secrets are not flashable: " + "; ".join(problems))


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

    if any(path.startswith("scripts/") or path.startswith(".github/workflows/") for path in path_strings):
        impacted.update(spec.name for spec in specs)

    return sorted(impacted)


def matrix_payload(devices: Iterable[str]) -> str:
    return json.dumps({"include": [{"device": device} for device in devices]})


STABLE_VERSION_RE = re.compile(r"^v(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)$")
EDGE_VERSION_RE = re.compile(r"^edge-(?P<created>\d{8}T\d{6}Z)-(?P<sha>[0-9a-f]{7,40})$")


def stable_version_key(version: str) -> tuple[int, int, int] | None:
    match = STABLE_VERSION_RE.fullmatch(version)
    if not match:
        return None
    return (int(match.group("major")), int(match.group("minor")), int(match.group("patch")))


def is_edge_version(version: str) -> bool:
    return EDGE_VERSION_RE.fullmatch(version) is not None


def firmware_channel(version: str) -> str:
    if stable_version_key(version) is not None:
        return "stable"
    if is_edge_version(version):
        return "edge"
    raise ValueError(f"unsupported firmware version format: {version}")


def edge_version(created_utc: str, source_sha: str) -> str:
    if not re.fullmatch(r"\d{8}T\d{6}Z", created_utc):
        raise ValueError(f"edge timestamp must be YYYYMMDDTHHMMSSZ: {created_utc}")
    short_sha = source_sha[:12]
    if not re.fullmatch(r"[0-9a-f]{7,40}", short_sha):
        raise ValueError(f"source sha must be lowercase hex: {source_sha}")
    return f"edge-{created_utc}-{short_sha}"

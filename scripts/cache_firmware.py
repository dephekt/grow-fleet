#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import time
from pathlib import Path

from fleetlib import device_names, device_spec, firmware_artifacts, sha256_file


DEFAULT_CACHE_ROOT = Path("/runner-cache/grow-fleet")
FIRMWARE_DIR = "firmware"
COMPLETE_FILENAME = "complete.json"
SETUP_ERROR = 2
MISS = 1


class CacheSetupError(RuntimeError):
    pass


def cache_root() -> Path:
    return Path(os.environ.get("FIRMWARE_CACHE_ROOT", DEFAULT_CACHE_ROOT))


def fail(message: str, code: int) -> None:
    print(message, file=sys.stderr)
    raise SystemExit(code)


def safe_component(value: str, label: str) -> str:
    if not value or value in {".", ".."} or "/" in value or "\\" in value:
        raise ValueError(f"{label} must be a single path component: {value!r}")
    return value


def require_cache_root(root: Path) -> None:
    if not root.exists():
        raise CacheSetupError(
            f"firmware cache root does not exist: {root}. "
            "Mount /srv/forgejo-runner/cache/grow-fleet at /runner-cache/grow-fleet."
        )
    if not root.is_dir():
        raise CacheSetupError(f"firmware cache root is not a directory: {root}")
    probe = root / f".write-test.{os.getpid()}.{time.time_ns()}"
    try:
        with probe.open("w", encoding="utf-8") as handle:
            handle.write("ok\n")
        probe.unlink()
    except OSError as exc:
        raise CacheSetupError(f"firmware cache root is not writable: {root}") from exc


def cache_device_dir(root: Path, sha: str, device: str) -> Path:
    return root / FIRMWARE_DIR / safe_component(sha, "sha") / safe_component(device, "device")


def complete_marker(path: Path) -> Path:
    return path / COMPLETE_FILENAME


def cache_artifact_paths(path: Path) -> dict[str, Path]:
    return {
        "ota": path / "firmware.ota.bin",
        "factory": path / "firmware.factory.bin",
    }


def cache_hit(path: Path) -> bool:
    marker = complete_marker(path)
    artifacts = cache_artifact_paths(path)
    return marker.exists() and all(artifact.exists() for artifact in artifacts.values())


def select_devices(args: argparse.Namespace) -> list[str]:
    if args.all and args.devices:
        raise ValueError("pass --all or explicit devices, not both")
    if args.all:
        return device_names(release_only=False)
    if not args.devices:
        raise ValueError("pass --all or at least one device")
    for device in args.devices:
        device_spec(device)
    return args.devices


def copy_atomic(source: Path, destination: Path) -> None:
    if not source.exists():
        raise FileNotFoundError(f"missing firmware artifact: {source}")
    destination.parent.mkdir(parents=True, exist_ok=True)
    tmp = destination.with_name(f"{destination.name}.tmp.{os.getpid()}.{time.time_ns()}")
    shutil.copy2(source, tmp)
    os.replace(tmp, destination)


def store_device(root: Path, sha: str, device: str) -> None:
    source_artifacts = firmware_artifacts(device)
    destination = cache_device_dir(root, sha, device)
    destination.mkdir(parents=True, exist_ok=True)

    cached_artifacts = cache_artifact_paths(destination)
    copy_atomic(source_artifacts["ota"], cached_artifacts["ota"])
    copy_atomic(source_artifacts["factory"], cached_artifacts["factory"])

    metadata = {
        "device": device,
        "sha": sha,
        "stored_at": int(time.time()),
        "sha256": {
            cached_artifacts["ota"].name: sha256_file(cached_artifacts["ota"]),
            cached_artifacts["factory"].name: sha256_file(cached_artifacts["factory"]),
        },
    }
    tmp_marker = destination / f"{COMPLETE_FILENAME}.tmp.{os.getpid()}.{time.time_ns()}"
    tmp_marker.write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.replace(tmp_marker, complete_marker(destination))
    print(f"stored firmware cache for {device} at {destination}")


def restore_device(root: Path, sha: str, device: str, wait_seconds: int) -> bool:
    device_spec(device)
    source = cache_device_dir(root, sha, device)
    deadline = time.monotonic() + wait_seconds
    printed_wait = False

    while True:
        if cache_hit(source):
            cached_artifacts = cache_artifact_paths(source)
            build_artifacts = firmware_artifacts(device)
            copy_atomic(cached_artifacts["ota"], build_artifacts["ota"])
            copy_atomic(cached_artifacts["factory"], build_artifacts["factory"])
            print(f"restored firmware cache for {device} from {source}")
            return True

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            print(f"firmware cache miss for {device} at {source}", file=sys.stderr)
            return False
        if not printed_wait:
            print(
                f"waiting up to {wait_seconds}s for firmware cache for {device} at {source}",
                file=sys.stderr,
            )
            printed_wait = True
        time.sleep(min(15, max(1, remaining)))


def newest_mtime(path: Path) -> float:
    newest = path.stat().st_mtime
    for child in path.rglob("*"):
        try:
            newest = max(newest, child.stat().st_mtime)
        except FileNotFoundError:
            continue
    return newest


def prune(root: Path, max_age_days: int, keep_shas: int) -> None:
    firmware_root = root / FIRMWARE_DIR
    if not firmware_root.exists():
        return

    entries: list[tuple[float, Path]] = []
    for child in firmware_root.iterdir():
        if child.is_dir():
            entries.append((newest_mtime(child), child))

    entries.sort(key=lambda item: item[0], reverse=True)
    protected = {path for _, path in entries[:keep_shas]}
    cutoff = time.time() - (max_age_days * 24 * 60 * 60)

    for mtime, path in entries:
        if path in protected or mtime >= cutoff:
            continue
        shutil.rmtree(path)
        print(f"pruned firmware cache {path}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Manage runner-local compiled firmware cache.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    store_parser = subparsers.add_parser("store", help="Store compiled firmware in the cache.")
    store_parser.add_argument("--sha", required=True, help="Commit SHA cache key.")
    store_parser.add_argument("--all", action="store_true", help="Store all devices.")
    store_parser.add_argument("devices", nargs="*", help="Devices to store.")

    restore_parser = subparsers.add_parser("restore", help="Restore compiled firmware from the cache.")
    restore_parser.add_argument("--sha", required=True, help="Commit SHA cache key.")
    restore_parser.add_argument(
        "--wait-seconds",
        type=int,
        default=0,
        help="Seconds to wait for another job to populate the cache.",
    )
    restore_parser.add_argument("device", help="Device to restore.")

    prune_parser = subparsers.add_parser("prune", help="Prune old cached firmware.")
    prune_parser.add_argument("--max-age-days", type=int, required=True, help="Maximum cache age in days.")
    prune_parser.add_argument("--keep-shas", type=int, required=True, help="Minimum recent SHA dirs to keep.")

    return parser


def main() -> None:
    parser = build_parser()
    args = parser.parse_args()
    root = cache_root()

    try:
        require_cache_root(root)
        if args.command == "store":
            for device in select_devices(args):
                store_device(root, args.sha, device)
        elif args.command == "restore":
            if args.wait_seconds < 0:
                raise ValueError("--wait-seconds must be nonnegative")
            hit = restore_device(root, args.sha, args.device, args.wait_seconds)
            if not hit:
                raise SystemExit(MISS)
        elif args.command == "prune":
            if args.max_age_days < 0:
                raise ValueError("--max-age-days must be nonnegative")
            if args.keep_shas < 0:
                raise ValueError("--keep-shas must be nonnegative")
            prune(root, args.max_age_days, args.keep_shas)
        else:
            parser.error(f"unknown command: {args.command}")
    except CacheSetupError as exc:
        fail(str(exc), SETUP_ERROR)
    except (FileNotFoundError, KeyError, ValueError) as exc:
        fail(str(exc), SETUP_ERROR)


if __name__ == "__main__":
    main()

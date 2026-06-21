#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import shutil
from datetime import UTC, datetime
from pathlib import Path

from fleetlib import (
    assert_flashable_secrets,
    capture,
    device_spec,
    esphome_version,
    firmware_artifacts,
    firmware_channel,
    fleet_component_dependency,
    md5_file,
    sha256_file,
    stable_version_key,
)


def generated_timestamp() -> str:
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def stable_firmware_tags(tags: list[str], device: str) -> list[tuple[tuple[int, int, int], str, str]]:
    prefix = f"firmware/{device}/"
    found: list[tuple[tuple[int, int, int], str, str]] = []
    for tag in tags:
        if not tag.startswith(prefix):
            continue
        version = tag.removeprefix(prefix)
        key = stable_version_key(version)
        if key is not None:
            found.append((key, version, tag))
    return sorted(found)


def previous_stable_tag(tags: list[str], device: str, version: str) -> str | None:
    current_key = stable_version_key(version)
    if current_key is None:
        return None
    previous = [tag for key, _, tag in stable_firmware_tags(tags, device) if key < current_key]
    return previous[-1] if previous else None


def latest_stable_tag(tags: list[str], device: str) -> str | None:
    stable_tags = stable_firmware_tags(tags, device)
    return stable_tags[-1][2] if stable_tags else None


def git_tags() -> list[str]:
    output = capture(["git", "tag", "--list", "firmware/*"])
    return [line.strip() for line in output.splitlines() if line.strip()]


def git_commits(base_ref: str | None, head_ref: str, limit: int = 30) -> list[dict[str, str]]:
    revision = f"{base_ref}..{head_ref}" if base_ref else head_ref
    output = capture(["git", "log", f"--max-count={limit}", "--format=%H%x00%s", revision])
    commits: list[dict[str, str]] = []
    for line in output.splitlines():
        if "\x00" not in line:
            continue
        sha, subject = line.split("\x00", 1)
        commits.append({"sha": sha, "subject": subject})
    return commits


def release_metadata(device: str, channel: str, version: str, source_sha: str) -> dict[str, object]:
    tags = git_tags()
    if channel == "stable":
        base_tag = previous_stable_tag(tags, device, version)
    elif channel == "edge":
        base_tag = latest_stable_tag(tags, device)
    else:
        raise ValueError(f"unsupported firmware channel: {channel}")

    commits = git_commits(base_tag, source_sha)
    if base_tag:
        summary = f"{len(commits)} commits since {base_tag}"
    else:
        summary = f"Initial {channel} firmware package for {device}"

    return {
        "release_url": release_url(device, channel, version, source_sha),
        "release_summary": summary,
        "changelog": {
            "base_tag": base_tag,
            "target_sha": source_sha,
            "commits": commits,
        },
    }


def release_url(device: str, channel: str, version: str, source_sha: str) -> str:
    if channel == "stable":
        return f"https://codeberg.org/stackdrift/grow-fleet/src/tag/firmware/{device}/{version}"
    return f"https://codeberg.org/stackdrift/grow-fleet/src/commit/{source_sha}"


def package_device(
    name: str,
    version: str,
    source_sha: str,
    dist_root: Path,
    channel: str | None = None,
    build_profile: str = "site-private",
    flashable: bool = True,
) -> Path:
    spec = device_spec(name)
    resolved_channel = channel or firmware_channel(version)
    artifacts = firmware_artifacts(name)
    ota_source = artifacts["ota"]
    factory_source = artifacts["factory"]
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
    metadata = release_metadata(name, resolved_channel, version, source_sha)
    manifest = {
        "schema": "grow-firmware-package.v1",
        "channel": resolved_channel,
        "build_profile": build_profile,
        "flashable": flashable,
        "device": name,
        "node_id": spec.node_id,
        "project_name": spec.project_name,
        "package_owner": spec.package_owner,
        "package": spec.package,
        "version": version,
        "source_sha": source_sha,
        "chip_family": spec.chip_family,
        "generated_at": generated_timestamp(),
        "component_dependency": fleet_component_dependency(),
        "esphome_version": esphome_version(),
        "artifact_filenames": artifact_filenames,
        "md5": {
            ota_dest.name: md5_file(ota_dest),
            factory_dest.name: md5_file(factory_dest),
        },
        "sha256": {
            ota_dest.name: sha256_file(ota_dest),
            factory_dest.name: sha256_file(factory_dest),
        },
        **metadata,
    }

    manifest_path = device_dist / f"{name}.manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return manifest_path


def main() -> None:
    parser = argparse.ArgumentParser(description="Package a compiled firmware device.")
    parser.add_argument("device", help="Device name to package.")
    parser.add_argument("--version", required=True, help="Release or build version.")
    parser.add_argument(
        "--channel",
        choices=["stable", "edge"],
        help="Firmware channel. Defaults from --version.",
    )
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
    parser.add_argument(
        "--build-profile",
        default="site-private",
        choices=["site-private", "ci-placeholder"],
        help="Secret/build profile recorded in the manifest.",
    )
    parser.add_argument(
        "--require-flashable-secrets",
        action="store_true",
        help="Reject packaging if devices/secrets.yaml still contains compile-only placeholder values.",
    )
    args = parser.parse_args()

    if args.require_flashable_secrets:
        assert_flashable_secrets()

    source_sha = args.source_sha or capture(["git", "rev-parse", "HEAD"])
    manifest_path = package_device(
        args.device,
        args.version,
        source_sha,
        Path(args.dist_root),
        channel=args.channel,
        build_profile=args.build_profile,
        flashable=args.build_profile == "site-private",
    )
    print(manifest_path)


if __name__ == "__main__":
    main()

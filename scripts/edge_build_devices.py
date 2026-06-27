#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import subprocess
import sys

from edge_changelog_base import download_oci_manifest, latest_edge_package
from fleetlib import changed_paths, device_names, device_spec, impacted_devices
from publish_packages import DEFAULT_OCI_OWNER, DEFAULT_OCI_PACKAGE_PREFIX, DEFAULT_OCI_REGISTRY, list_oci_tags


def commit_exists(ref: str) -> bool:
    completed = subprocess.run(
        ["git", "cat-file", "-e", f"{ref}^{{commit}}"],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return completed.returncode == 0


def latest_edge_manifest(
    device: str,
    registry: str = DEFAULT_OCI_REGISTRY,
    owner: str = DEFAULT_OCI_OWNER,
    package_prefix: str = DEFAULT_OCI_PACKAGE_PREFIX,
    exclude_version: str | None = None,
) -> dict[str, object] | None:
    spec = device_spec(device)
    packages = [
        {"name": spec.package, "version": version}
        for version in list_oci_tags(registry, owner, package_prefix, spec.package)
    ]
    latest = latest_edge_package(packages, exclude_version=exclude_version)
    if not latest:
        return None

    version = str(latest["version"])
    return download_oci_manifest(
        registry,
        owner,
        package_prefix,
        spec.package,
        version,
        f"{device}.manifest.json",
    )


def should_build_device(
    device: str,
    head: str,
    registry: str = DEFAULT_OCI_REGISTRY,
    owner: str = DEFAULT_OCI_OWNER,
    package_prefix: str = DEFAULT_OCI_PACKAGE_PREFIX,
    exclude_version: str | None = None,
) -> bool:
    try:
        manifest = latest_edge_manifest(
            device,
            registry=registry,
            owner=owner,
            package_prefix=package_prefix,
            exclude_version=exclude_version,
        )
    except Exception as exc:
        print(f"::notice::Building {device}: unable to read latest edge package: {exc}", file=sys.stderr)
        return True

    if manifest is None:
        print(f"::notice::Building {device}: no previous edge package found", file=sys.stderr)
        return True

    source_sha = manifest.get("source_sha")
    if not isinstance(source_sha, str) or not source_sha:
        print(f"::notice::Building {device}: latest edge manifest is missing source_sha", file=sys.stderr)
        return True
    if not commit_exists(source_sha):
        print(f"::notice::Building {device}: previous source commit {source_sha} is unavailable", file=sys.stderr)
        return True

    paths = changed_paths(source_sha, head)
    return device in set(impacted_devices(paths))


def edge_build_devices(
    head: str,
    registry: str = DEFAULT_OCI_REGISTRY,
    owner: str = DEFAULT_OCI_OWNER,
    package_prefix: str = DEFAULT_OCI_PACKAGE_PREFIX,
    exclude_version: str | None = None,
) -> list[str]:
    return [
        device
        for device in device_names(release_only=True)
        if should_build_device(
            device,
            head=head,
            registry=registry,
            owner=owner,
            package_prefix=package_prefix,
            exclude_version=exclude_version,
        )
    ]


def main() -> None:
    parser = argparse.ArgumentParser(description="List release devices that need a new edge firmware build.")
    parser.add_argument("--head", default="HEAD", help="Target revision to compare against published edge packages.")
    parser.add_argument("--exclude-version", default=None, help="Edge version to ignore when selecting previous packages.")
    parser.add_argument("--oci-registry", default=DEFAULT_OCI_REGISTRY, help="OCI registry.")
    parser.add_argument("--oci-owner", default=DEFAULT_OCI_OWNER, help="OCI registry owner.")
    parser.add_argument("--oci-package-prefix", default=DEFAULT_OCI_PACKAGE_PREFIX, help="OCI package prefix.")
    parser.add_argument("--json", action="store_true", help="Emit a JSON array.")
    args = parser.parse_args()

    devices = edge_build_devices(
        args.head,
        registry=args.oci_registry,
        owner=args.oci_owner,
        package_prefix=args.oci_package_prefix,
        exclude_version=args.exclude_version,
    )
    if args.json:
        print(json.dumps(devices))
        return
    print(" ".join(devices))


if __name__ == "__main__":
    main()

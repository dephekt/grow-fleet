#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
from urllib.parse import quote
from urllib.request import Request, urlopen

from fleetlib import device_spec
from publish_packages import (
    DEFAULT_PACKAGE_USER,
    EDGE_VERSION_RE,
    PRIVATE_PACKAGE_USER,
    authorization_header,
    list_generic_packages,
)


def edge_packages(packages: list[dict[str, object]], exclude_version: str | None = None) -> list[dict[str, object]]:
    matches = [
        package
        for package in packages
        if isinstance(package.get("version"), str)
        and EDGE_VERSION_RE.fullmatch(str(package["version"]))
        and package.get("version") != exclude_version
    ]
    return sorted(matches, key=lambda package: str(package["version"]), reverse=True)


def latest_edge_package(packages: list[dict[str, object]], exclude_version: str | None = None) -> dict[str, object] | None:
    matches = edge_packages(packages, exclude_version=exclude_version)
    return matches[0] if matches else None


def package_manifest_url(base_url: str, package_user: str, package: str, version: str, manifest_filename: str) -> str:
    return (
        f"{base_url.rstrip('/')}/api/packages/"
        f"{quote(package_user, safe='')}/generic/"
        f"{quote(package, safe='')}/"
        f"{quote(version, safe='')}/"
        f"{quote(manifest_filename, safe='')}"
    )


def download_manifest(
    base_url: str,
    package_user: str,
    package: str,
    version: str,
    manifest_filename: str,
    auth_user: str | None,
    token: str | None,
    auth_scheme: str,
) -> dict[str, object]:
    headers = {}
    if token:
        if not auth_user:
            raise ValueError("auth_user is required when token is provided")
        headers["Authorization"] = authorization_header(auth_user, token, auth_scheme)
    request = Request(package_manifest_url(base_url, package_user, package, version, manifest_filename), method="GET", headers=headers)
    with urlopen(request) as response:
        payload = json.loads(response.read().decode("utf-8"))
    if not isinstance(payload, dict):
        raise ValueError("package manifest response must be an object")
    return payload


def resolve_auth(package_user: str, explicit_auth_user: str | None) -> tuple[str | None, str | None, str]:
    package_token = os.environ.get("PACKAGE_TOKEN")
    forgejo_token = os.environ.get("FORGEJO_TOKEN")
    token = package_token or forgejo_token
    auth_scheme = "basic" if package_token else "bearer"
    auth_user = explicit_auth_user
    if package_token and not auth_user:
        if package_user != DEFAULT_PACKAGE_USER:
            raise SystemExit("PACKAGE_AUTH_USER is required when reading an org package namespace")
        auth_user = package_user
    if package_token and package_user == PRIVATE_PACKAGE_USER and auth_user == package_user:
        raise SystemExit("PACKAGE_AUTH_USER must be the PAT-owning user, not stackdrift-firmware")
    if token and not auth_user:
        auth_user = package_user
    return auth_user, token, auth_scheme


def main() -> None:
    parser = argparse.ArgumentParser(description="Print the latest published edge package as changelog base key-value lines.")
    parser.add_argument("device", help="Fleet device name.")
    parser.add_argument("--exclude-version", default=None, help="Edge version to ignore when selecting the previous package.")
    parser.add_argument("--base-url", default="https://codeberg.org", help="Forgejo base URL.")
    parser.add_argument(
        "--package-user",
        default=os.environ.get("PACKAGE_USER"),
        help="Forgejo package namespace. Defaults to the fleet device package owner.",
    )
    parser.add_argument(
        "--auth-user",
        default=os.environ.get("PACKAGE_AUTH_USER"),
        help="Forgejo username for Basic auth. Required when PACKAGE_USER is an org.",
    )
    args = parser.parse_args()

    spec = device_spec(args.device)
    package_user = args.package_user or spec.package_owner
    auth_user, token, auth_scheme = resolve_auth(package_user, args.auth_user)
    packages = list_generic_packages(
        args.base_url,
        package_user,
        spec.package,
        auth_user=auth_user,
        token=token,
        auth_scheme=auth_scheme,
    )
    latest = latest_edge_package(packages, exclude_version=args.exclude_version)
    if not latest:
        return

    version = str(latest["version"])
    manifest = download_manifest(
        args.base_url,
        package_user,
        spec.package,
        version,
        f"{args.device}.manifest.json",
        auth_user,
        token,
        auth_scheme,
    )
    source_sha = manifest.get("source_sha")
    manifest_version = manifest.get("version", version)
    if not isinstance(source_sha, str) or not source_sha:
        raise ValueError(f"package manifest is missing source_sha for {spec.package} {version}")
    if not isinstance(manifest_version, str) or not manifest_version:
        raise ValueError(f"package manifest is missing version for {spec.package} {version}")

    print(f"version={manifest_version}")
    print(f"source_sha={source_sha}")


if __name__ == "__main__":
    main()

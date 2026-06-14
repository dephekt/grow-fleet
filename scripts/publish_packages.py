#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import json
import os
from pathlib import Path
from urllib.parse import quote
from urllib.request import Request, urlopen


DEFAULT_PACKAGE_USER = "stackdrift"


def upload_file(
    base_url: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    package_user: str,
    package: str,
    version: str,
    file_path: Path,
) -> None:
    target_url = (
        f"{base_url}/api/packages/"
        f"{quote(package_user, safe='')}/generic/"
        f"{quote(package, safe='')}/"
        f"{quote(version, safe='')}/"
        f"{quote(file_path.name, safe='')}"
    )
    data = file_path.read_bytes()
    if auth_scheme == "basic":
        auth_value = base64.b64encode(f"{auth_user}:{token}".encode("utf-8")).decode("ascii")
        authorization = f"Basic {auth_value}"
    elif auth_scheme == "bearer":
        authorization = f"Bearer {token}"
    else:
        raise ValueError(f"unsupported auth scheme: {auth_scheme}")

    request = Request(
        target_url,
        data=data,
        method="PUT",
        headers={
            "Authorization": authorization,
            "Content-Type": "application/octet-stream",
        },
    )
    with urlopen(request) as response:
        response.read()


def publish_device(
    dist_root: Path,
    device: str,
    package_user: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    base_url: str,
) -> None:
    device_dir = dist_root / device
    manifest_path = device_dir / f"{device}.manifest.json"
    if not manifest_path.exists():
        raise FileNotFoundError(f"missing manifest: {manifest_path}")

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    package = manifest["package"]
    version = manifest["version"]

    for filename in manifest["artifact_filenames"] + [manifest_path.name]:
        upload_file(
            base_url,
            auth_user,
            token,
            auth_scheme,
            package_user,
            package,
            version,
            device_dir / filename,
        )


def main() -> None:
    parser = argparse.ArgumentParser(description="Publish packaged firmware artifacts to Forgejo.")
    parser.add_argument("devices", nargs="+", help="Device names to publish.")
    parser.add_argument("--dist-root", default="dist", help="Directory containing packaged artifacts.")
    parser.add_argument(
        "--base-url",
        default="https://codeberg.org",
        help="Forgejo base URL.",
    )
    parser.add_argument(
        "--package-user",
        default=os.environ.get("PACKAGE_USER", DEFAULT_PACKAGE_USER),
        help="Forgejo package namespace.",
    )
    parser.add_argument(
        "--auth-user",
        default=os.environ.get("PACKAGE_AUTH_USER") or os.environ.get("PACKAGE_USER", DEFAULT_PACKAGE_USER),
        help="Forgejo username for Basic auth. Defaults to PACKAGE_AUTH_USER, then PACKAGE_USER.",
    )
    args = parser.parse_args()

    package_token = os.environ.get("PACKAGE_TOKEN")
    forgejo_token = os.environ.get("FORGEJO_TOKEN")
    token = package_token or forgejo_token
    if not token:
        raise SystemExit("PACKAGE_TOKEN or FORGEJO_TOKEN is required")
    auth_scheme = "basic" if package_token else "bearer"

    dist_root = Path(args.dist_root)
    for device in args.devices:
        publish_device(
            dist_root,
            device,
            args.package_user,
            args.auth_user,
            token,
            auth_scheme,
            args.base_url,
        )


if __name__ == "__main__":
    main()

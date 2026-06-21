#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import json
import os
import re
from pathlib import Path
from urllib.parse import quote
from urllib.request import Request, urlopen


DEFAULT_PACKAGE_USER = "stackdrift"
PACKAGE_LIST_PAGE_SIZE = 50
EDGE_VERSION_RE = re.compile(r"^edge-(?P<created>\d{8}T\d{6}Z)-(?P<sha>[0-9a-f]{7,40})$")


def authorization_header(auth_user: str, token: str, auth_scheme: str) -> str:
    if auth_scheme == "basic":
        auth_value = base64.b64encode(f"{auth_user}:{token}".encode("utf-8")).decode("ascii")
        return f"Basic {auth_value}"
    if auth_scheme == "bearer":
        return f"Bearer {token}"
    raise ValueError(f"unsupported auth scheme: {auth_scheme}")


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

    request = Request(
        target_url,
        data=data,
        method="PUT",
        headers={
            "Authorization": authorization_header(auth_user, token, auth_scheme),
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


def package_list_url(base_url: str, package_user: str, package: str, page: int, page_size: int) -> str:
    return (
        f"{base_url}/api/v1/packages/"
        f"{quote(package_user, safe='')}"
        f"?type=generic&q={quote(package, safe='')}"
        f"&page={page}&limit={page_size}"
    )


def has_next_page(link_header: str | None) -> bool:
    if not link_header:
        return False
    return any('rel="next"' in entry for entry in link_header.split(","))


def list_generic_packages(
    base_url: str,
    package_user: str,
    package: str,
    page_size: int = PACKAGE_LIST_PAGE_SIZE,
) -> list[dict[str, object]]:
    packages: list[dict[str, object]] = []
    page = 1
    while True:
        request = Request(package_list_url(base_url, package_user, package, page, page_size), method="GET")
        with urlopen(request) as response:
            payload = json.loads(response.read().decode("utf-8"))
            link_header = response.headers.get("Link")
        if not isinstance(payload, list):
            raise ValueError("package list response must be an array")
        packages.extend(item for item in payload if isinstance(item, dict) and item.get("name") == package)

        if link_header:
            if not has_next_page(link_header):
                break
        elif len(payload) < page_size:
            break
        page += 1
    return packages


def edge_cleanup_candidates(versions: list[str], keep: int) -> list[str]:
    if keep < 0:
        raise ValueError("keep must be nonnegative")
    edge_versions = [version for version in versions if EDGE_VERSION_RE.fullmatch(version)]
    edge_versions.sort(reverse=True)
    return edge_versions[keep:]


def delete_package_version(
    base_url: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    package_user: str,
    package: str,
    version: str,
) -> None:
    target_url = (
        f"{base_url}/api/packages/"
        f"{quote(package_user, safe='')}/generic/"
        f"{quote(package, safe='')}/"
        f"{quote(version, safe='')}"
    )
    request = Request(
        target_url,
        method="DELETE",
        headers={"Authorization": authorization_header(auth_user, token, auth_scheme)},
    )
    with urlopen(request) as response:
        response.read()


def prune_edge_packages(
    base_url: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    package_user: str,
    package: str,
    keep: int,
) -> list[str]:
    packages = list_generic_packages(base_url, package_user, package)
    versions = [str(item["version"]) for item in packages if "version" in item]
    candidates = edge_cleanup_candidates(versions, keep)
    for version in candidates:
        delete_package_version(base_url, auth_user, token, auth_scheme, package_user, package, version)
    return candidates


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
    parser.add_argument(
        "--prune-edge",
        action="store_true",
        help="Delete old edge package versions after publishing.",
    )
    parser.add_argument(
        "--keep-edge",
        type=int,
        default=int(os.environ.get("PACKAGE_KEEP_EDGE", "10")),
        help="Number of newest edge versions to retain per package when pruning.",
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
        if args.prune_edge:
            manifest_path = dist_root / device / f"{device}.manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            removed = prune_edge_packages(
                args.base_url,
                args.auth_user,
                token,
                auth_scheme,
                args.package_user,
                str(manifest["package"]),
                args.keep_edge,
            )
            for version in removed:
                print(f"pruned edge package {manifest['package']} {version}")


if __name__ == "__main__":
    main()

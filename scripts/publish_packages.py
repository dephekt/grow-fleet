#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import json
import os
import re
import subprocess
from pathlib import Path
from urllib.parse import quote
from urllib.request import Request, urlopen


DEFAULT_PACKAGE_USER = "stackdrift"
PRIVATE_PACKAGE_USER = "stackdrift-firmware"
DEFAULT_OCI_REGISTRY = "ghcr.io"
DEFAULT_OCI_OWNER = "dephekt"
DEFAULT_OCI_PACKAGE_PREFIX = "grow-fleet"
DEFAULT_OCI_SOURCE_URL = "https://github.com/dephekt/grow-fleet"
OCI_SOURCE_ANNOTATION = "org.opencontainers.image.source"
OCI_ARTIFACT_TYPE = "application/vnd.stackdrift.grow-firmware.v1"
OCI_MANIFEST_MEDIA_TYPE = "application/vnd.stackdrift.grow-firmware.manifest.v1+json"
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
    if manifest.get("flashable") is not True:
        raise ValueError(f"refusing to publish non-flashable manifest: {manifest_path}")
    package = manifest["package"]
    version = manifest["version"]

    preflight_package_access(base_url, auth_user, token, auth_scheme, package_user, package)

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
    auth_user: str | None = None,
    token: str | None = None,
    auth_scheme: str = "basic",
) -> list[dict[str, object]]:
    packages: list[dict[str, object]] = []
    page = 1
    while True:
        headers = {}
        if token:
            if not auth_user:
                raise ValueError("auth_user is required when token is provided")
            headers["Authorization"] = authorization_header(auth_user, token, auth_scheme)
        request = Request(package_list_url(base_url, package_user, package, page, page_size), method="GET", headers=headers)
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


def preflight_package_access(
    base_url: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    package_user: str,
    package: str,
) -> None:
    list_generic_packages(
        base_url,
        package_user,
        package,
        auth_user=auth_user,
        token=token,
        auth_scheme=auth_scheme,
    )


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
    packages = list_generic_packages(
        base_url,
        package_user,
        package,
        auth_user=auth_user,
        token=token,
        auth_scheme=auth_scheme,
    )
    versions = [str(item["version"]) for item in packages if "version" in item]
    candidates = edge_cleanup_candidates(versions, keep)
    for version in candidates:
        delete_package_version(base_url, auth_user, token, auth_scheme, package_user, package, version)
    return candidates


def oci_package_name(package_prefix: str, package: str) -> str:
    return f"{package_prefix}-{package}".lower()


def oci_repository(registry: str, owner: str, package_prefix: str, package: str) -> str:
    return f"{registry.rstrip('/')}/{owner}/{oci_package_name(package_prefix, package)}"


def oci_ref(registry: str, owner: str, package_prefix: str, package: str, version: str) -> str:
    return f"{oci_repository(registry, owner, package_prefix, package)}:{version}"


def publish_device_oci(
    dist_root: Path,
    device: str,
    registry: str,
    owner: str,
    package_prefix: str,
    source_url: str | None = None,
) -> None:
    device_dir = dist_root / device
    manifest_path = device_dir / f"{device}.manifest.json"
    if not manifest_path.exists():
        raise FileNotFoundError(f"missing manifest: {manifest_path}")

    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    if manifest.get("flashable") is not True:
        raise ValueError(f"refusing to publish non-flashable manifest: {manifest_path}")

    package = str(manifest["package"])
    version = str(manifest["version"])
    target = oci_ref(registry, owner, package_prefix, package, version)
    args = ["oras", "push", target, "--artifact-type", OCI_ARTIFACT_TYPE]
    if source_url:
        args.extend(["--annotation", f"{OCI_SOURCE_ANNOTATION}={source_url}"])
    for filename in manifest["artifact_filenames"]:
        args.append(f"{filename}:application/octet-stream")
    args.append(f"{manifest_path.name}:{OCI_MANIFEST_MEDIA_TYPE}")
    subprocess.run(args, check=True, cwd=device_dir)


def list_oci_tags(registry: str, owner: str, package_prefix: str, package: str) -> list[str]:
    repository = oci_repository(registry, owner, package_prefix, package)
    completed = subprocess.run(
        ["oras", "repo", "tags", repository],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if completed.returncode != 0:
        return []
    return [line.strip() for line in completed.stdout.splitlines() if line.strip() and not line.startswith("Tags for ")]


def prune_edge_oci_packages(registry: str, owner: str, package_prefix: str, package: str, keep: int) -> list[str]:
    candidates = edge_cleanup_candidates(list_oci_tags(registry, owner, package_prefix, package), keep)
    for version in candidates:
        subprocess.run(
            ["oras", "manifest", "delete", "--force", oci_ref(registry, owner, package_prefix, package, version)],
            check=True,
        )
    return candidates


def main() -> None:
    parser = argparse.ArgumentParser(description="Publish packaged firmware artifacts.")
    parser.add_argument("devices", nargs="+", help="Device names to publish.")
    parser.add_argument("--dist-root", default="dist", help="Directory containing packaged artifacts.")
    parser.add_argument(
        "--provider",
        choices=["ghcr-oci", "forgejo-generic"],
        default=os.environ.get("PACKAGE_PROVIDER", "ghcr-oci"),
        help="Artifact backend to publish to.",
    )
    parser.add_argument(
        "--oci-registry",
        default=os.environ.get("OCI_REGISTRY", DEFAULT_OCI_REGISTRY),
        help="OCI registry for firmware artifacts.",
    )
    parser.add_argument(
        "--oci-owner",
        default=os.environ.get("OCI_OWNER", DEFAULT_OCI_OWNER),
        help="OCI registry owner/namespace for firmware artifacts.",
    )
    parser.add_argument(
        "--oci-package-prefix",
        default=os.environ.get("OCI_PACKAGE_PREFIX", DEFAULT_OCI_PACKAGE_PREFIX),
        help="Prefix for per-device OCI firmware package names.",
    )
    parser.add_argument(
        "--oci-source-url",
        default=os.environ.get("OCI_SOURCE_URL", DEFAULT_OCI_SOURCE_URL),
        help="Repository URL to attach as the OCI source annotation.",
    )
    parser.add_argument(
        "--base-url",
        default="https://codeberg.org",
        help="Forgejo base URL for forgejo-generic publishing.",
    )
    parser.add_argument(
        "--package-user",
        default=os.environ.get("PACKAGE_USER", DEFAULT_PACKAGE_USER),
        help="Forgejo package namespace.",
    )
    parser.add_argument(
        "--auth-user",
        default=os.environ.get("PACKAGE_AUTH_USER"),
        help="Forgejo username for Basic auth. Required when PACKAGE_USER is an org.",
    )
    parser.add_argument(
        "--preflight-only",
        action="store_true",
        help="Validate package namespace access from existing manifests without uploading artifacts.",
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

    dist_root = Path(args.dist_root)
    if args.provider == "ghcr-oci":
        for device in args.devices:
            publish_device_oci(
                dist_root,
                device,
                args.oci_registry,
                args.oci_owner,
                args.oci_package_prefix,
                args.oci_source_url,
            )
            if args.prune_edge:
                manifest_path = dist_root / device / f"{device}.manifest.json"
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
                removed = prune_edge_oci_packages(
                    args.oci_registry,
                    args.oci_owner,
                    args.oci_package_prefix,
                    str(manifest["package"]),
                    args.keep_edge,
                )
                for version in removed:
                    print(f"pruned edge package {manifest['package']} {version}")
        return

    package_token = os.environ.get("PACKAGE_TOKEN")
    forgejo_token = os.environ.get("FORGEJO_TOKEN")
    token = package_token or forgejo_token
    if not token:
        raise SystemExit("PACKAGE_TOKEN or FORGEJO_TOKEN is required")
    auth_scheme = "basic" if package_token else "bearer"
    auth_user = args.auth_user
    if package_token and not auth_user:
        if args.package_user != DEFAULT_PACKAGE_USER:
            raise SystemExit("PACKAGE_AUTH_USER is required when publishing to an org package namespace")
        auth_user = args.package_user
    if package_token and args.package_user == PRIVATE_PACKAGE_USER and auth_user == args.package_user:
        raise SystemExit("PACKAGE_AUTH_USER must be the PAT-owning user, not stackdrift-firmware")
    if not auth_user:
        auth_user = args.package_user

    for device in args.devices:
        if args.preflight_only:
            manifest_path = dist_root / device / f"{device}.manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            preflight_package_access(
                args.base_url,
                auth_user,
                token,
                auth_scheme,
                args.package_user,
                str(manifest["package"]),
            )
            continue
        publish_device(
            dist_root,
            device,
            args.package_user,
            auth_user,
            token,
            auth_scheme,
            args.base_url,
        )
        if args.prune_edge:
            manifest_path = dist_root / device / f"{device}.manifest.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            removed = prune_edge_packages(
                args.base_url,
                auth_user,
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

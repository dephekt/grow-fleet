#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import json
import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

DEFAULT_PACKAGE_USER = "stackdrift"
PRIVATE_PACKAGE_USER = "stackdrift-firmware"
DEFAULT_OCI_REGISTRY = "ghcr.io"
DEFAULT_OCI_OWNER = "dephekt"
DEFAULT_OCI_PACKAGE_PREFIX = "grow-fleet"
GITHUB_API_BASE = "https://api.github.com"
DEFAULT_OCI_SOURCE_URL = ""
OCI_SOURCE_ANNOTATION = "org.opencontainers.image.source"
OCI_FIRMWARE_ARTIFACT_TYPE = "application/vnd.stackdrift.grow-firmware.v1"
OCI_FIRMWARE_MANIFEST_MEDIA_TYPE = "application/vnd.stackdrift.grow-firmware.manifest.v1+json"
OCI_PUBLISHER_ARTIFACT_TYPE = "application/vnd.stackdrift.grow-publisher.v1"
OCI_PUBLISHER_MANIFEST_MEDIA_TYPE = "application/vnd.stackdrift.grow-publisher.manifest.v1+json"
PACKAGE_LIST_PAGE_SIZE = 50
EDGE_VERSION_RE = re.compile(r"^edge-(?P<created>\d{8}T\d{6}Z)-(?P<sha>[0-9a-f]{7,40})$")


@dataclass(frozen=True)
class ArtifactKind:
    """What a ``dist/<name>/`` tree holds, and how it is pushed.

    This module grew up serving exactly one payload — flashable ESPHome firmware —
    so the OCI artifact type, the manifest layer's media type, and the "is this
    actually flashable?" guard were all module constants. The manifest's
    ``flashable`` boolean (written by ``package_device.py`` and ``build_arduino.py``)
    is the seam: making the kind explicit lets a payload that is not firmware —
    the Go publisher daemons under ``publishers/`` — reuse the dist layout, the
    edge tag scheme, and the GHCR pruning without inheriting a guard that cannot
    mean anything for it.

    Everything below the kind (``oci_ref``, ``list_oci_tags``,
    ``edge_cleanup_candidates``, ``list_ghcr_package_versions``,
    ``delete_ghcr_package_version``) is already payload-agnostic and takes the
    package name as data, so only these three values had to be lifted.
    """

    name: str
    artifact_type: str
    manifest_media_type: str
    require_flashable: bool


FIRMWARE_KIND = ArtifactKind(
    name="firmware",
    artifact_type=OCI_FIRMWARE_ARTIFACT_TYPE,
    manifest_media_type=OCI_FIRMWARE_MANIFEST_MEDIA_TYPE,
    require_flashable=True,
)
BINARY_KIND = ArtifactKind(
    name="binary",
    artifact_type=OCI_PUBLISHER_ARTIFACT_TYPE,
    manifest_media_type=OCI_PUBLISHER_MANIFEST_MEDIA_TYPE,
    require_flashable=False,
)
KINDS = {kind.name: kind for kind in (FIRMWARE_KIND, BINARY_KIND)}
DEFAULT_KIND = FIRMWARE_KIND


def resolve_kind(name: str) -> ArtifactKind:
    """Look up a kind by name, diagnosing a bad one rather than raising KeyError.

    ``--kind`` declares ``choices``, but argparse does not validate a default it
    took from the environment, so a typo in ``PACKAGE_KIND`` reaches here.
    """
    try:
        return KINDS[name]
    except KeyError:
        known = ", ".join(sorted(KINDS))
        raise SystemExit(f"unknown artifact kind: {name} (choose one of: {known})") from None


def authorization_header(auth_user: str, token: str, auth_scheme: str) -> str:
    if auth_scheme == "basic":
        auth_value = base64.b64encode(f"{auth_user}:{token}".encode()).decode("ascii")
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


def manifest_path(dist_root: Path, name: str) -> Path:
    return dist_root / name / f"{name}.manifest.json"


def read_manifest(dist_root: Path, name: str) -> dict[str, Any]:
    path = manifest_path(dist_root, name)
    if not path.exists():
        raise FileNotFoundError(f"missing manifest: {path}")
    manifest: dict[str, Any] = json.loads(path.read_text(encoding="utf-8"))
    return manifest


def load_publishable_manifest(dist_root: Path, name: str, kind: ArtifactKind) -> dict[str, Any]:
    """Read the manifest and refuse it if it does not match ``kind``.

    Firmware must declare ``flashable: true`` — a ci-placeholder build carries
    compile-only secrets and must never reach a device. The check is symmetric on
    purpose: a firmware manifest published under a non-firmware kind would land
    under the wrong artifact type and grow-app would simply not see it, which is
    a silent failure rather than a loud one.
    """
    path = manifest_path(dist_root, name)
    manifest = read_manifest(dist_root, name)
    if kind.require_flashable:
        if manifest.get("flashable") is not True:
            raise ValueError(f"refusing to publish non-flashable manifest: {path}")
    elif "flashable" in manifest:
        raise ValueError(f"refusing to publish a firmware manifest as {kind.name}: {path}")
    return manifest


def publish_artifact(
    dist_root: Path,
    name: str,
    package_user: str,
    auth_user: str,
    token: str,
    auth_scheme: str,
    base_url: str,
    *,
    kind: ArtifactKind = DEFAULT_KIND,
) -> None:
    manifest = load_publishable_manifest(dist_root, name, kind)
    package = manifest["package"]
    version = manifest["version"]

    preflight_package_access(base_url, auth_user, token, auth_scheme, package_user, package)

    artifact_dir = dist_root / name
    for filename in manifest["artifact_filenames"] + [manifest_path(dist_root, name).name]:
        upload_file(
            base_url,
            auth_user,
            token,
            auth_scheme,
            package_user,
            package,
            version,
            artifact_dir / filename,
        )


def package_list_url(
    base_url: str, package_user: str, package: str, page: int, page_size: int
) -> str:
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
        request = Request(
            package_list_url(base_url, package_user, package, page, page_size),
            method="GET",
            headers=headers,
        )
        with urlopen(request) as response:
            payload = json.loads(response.read().decode("utf-8"))
            link_header = response.headers.get("Link")
        if not isinstance(payload, list):
            raise ValueError("package list response must be an array")
        packages.extend(
            item for item in payload if isinstance(item, dict) and item.get("name") == package
        )

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
        delete_package_version(
            base_url, auth_user, token, auth_scheme, package_user, package, version
        )
    return candidates


def oci_package_name(package_prefix: str, package: str) -> str:
    return f"{package_prefix}-{package}".lower()


def oci_repository(registry: str, owner: str, package_prefix: str, package: str) -> str:
    return f"{registry.rstrip('/')}/{owner}/{oci_package_name(package_prefix, package)}"


def oci_ref(registry: str, owner: str, package_prefix: str, package: str, version: str) -> str:
    return f"{oci_repository(registry, owner, package_prefix, package)}:{version}"


def publish_artifact_oci(
    dist_root: Path,
    name: str,
    registry: str,
    owner: str,
    package_prefix: str,
    source_url: str | None = None,
    *,
    kind: ArtifactKind = DEFAULT_KIND,
) -> None:
    manifest = load_publishable_manifest(dist_root, name, kind)

    package = str(manifest["package"])
    version = str(manifest["version"])
    target = oci_ref(registry, owner, package_prefix, package, version)
    args = ["oras", "push", target, "--artifact-type", kind.artifact_type]
    if source_url:
        args.extend(["--annotation", f"{OCI_SOURCE_ANNOTATION}={source_url}"])
    for filename in manifest["artifact_filenames"]:
        args.append(f"{filename}:application/octet-stream")
    args.append(f"{manifest_path(dist_root, name).name}:{kind.manifest_media_type}")
    subprocess.run(args, check=True, cwd=dist_root / name)


def list_oci_tags(registry: str, owner: str, package_prefix: str, package: str) -> list[str]:
    repository = oci_repository(registry, owner, package_prefix, package)
    completed = subprocess.run(
        ["oras", "repo", "tags", repository],
        check=False,
        capture_output=True,
        text=True,
    )
    if completed.returncode != 0:
        return []
    return [
        line.strip()
        for line in completed.stdout.splitlines()
        if line.strip() and not line.startswith("Tags for ")
    ]


def github_packages_token() -> str | None:
    return os.environ.get("GHCR_TOKEN") or os.environ.get("GITHUB_TOKEN")


def _github_api_request(url: str, token: str, method: str = "GET") -> Request:
    return Request(
        url,
        method=method,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )


def list_ghcr_package_versions(
    owner: str, package_name: str, token: str
) -> list[dict[str, object]]:
    """List container package versions for a user-owned GHCR package.

    GHCR does not implement the OCI registry manifest-delete endpoint, so
    pruning must go through the GitHub Packages REST API instead of oras.
    """
    versions: list[dict[str, object]] = []
    page = 1
    while True:
        url = (
            f"{GITHUB_API_BASE}/users/{quote(owner, safe='')}"
            f"/packages/container/{quote(package_name, safe='')}/versions"
            f"?per_page=100&page={page}"
        )
        with urlopen(_github_api_request(url, token)) as response:
            payload = json.loads(response.read().decode("utf-8"))
        if not isinstance(payload, list) or not payload:
            break
        versions.extend(item for item in payload if isinstance(item, dict))
        if len(payload) < 100:
            break
        page += 1
    return versions


def delete_ghcr_package_version(
    owner: str, package_name: str, version_id: object, token: str
) -> None:
    url = (
        f"{GITHUB_API_BASE}/users/{quote(owner, safe='')}"
        f"/packages/container/{quote(package_name, safe='')}"
        f"/versions/{quote(str(version_id), safe='')}"
    )
    with urlopen(_github_api_request(url, token, method="DELETE")) as response:
        response.read()


def _version_ids_by_tag(versions: list[dict[str, object]]) -> dict[str, object]:
    mapping: dict[str, object] = {}
    for version in versions:
        metadata = version.get("metadata")
        container = metadata.get("container") if isinstance(metadata, dict) else None
        tags = container.get("tags") if isinstance(container, dict) else None
        if not isinstance(tags, list):
            continue
        for tag in tags:
            if isinstance(tag, str):
                mapping[tag] = version.get("id")
    return mapping


def prune_edge_oci_packages(
    registry: str, owner: str, package_prefix: str, package: str, keep: int
) -> list[str]:
    """Delete the oldest edge tags beyond ``keep`` via the GitHub Packages API.

    Best-effort: any failure (missing token, insufficient scope, transient API
    error) is reported as a warning and never aborts publishing. The prior
    ``oras manifest delete`` approach always failed on GHCR with
    "unsupported: The operation is unsupported".
    """
    candidates = edge_cleanup_candidates(
        list_oci_tags(registry, owner, package_prefix, package), keep
    )
    if not candidates:
        return []

    token = github_packages_token()
    if not token:
        print(
            f"::warning::skipping edge prune for {package}: "
            "set GHCR_TOKEN (delete:packages) to enable pruning",
            flush=True,
        )
        return []

    package_name = oci_package_name(package_prefix, package)
    try:
        version_ids = _version_ids_by_tag(list_ghcr_package_versions(owner, package_name, token))
    except (HTTPError, URLError) as exc:
        print(
            f"::warning::skipping edge prune for {package}: cannot list package versions: {exc}",
            flush=True,
        )
        return []

    removed: list[str] = []
    for version in candidates:
        version_id = version_ids.get(version)
        if version_id is None:
            print(
                f"::warning::edge prune: no package version found for {package} {version}",
                flush=True,
            )
            continue
        try:
            delete_ghcr_package_version(owner, package_name, version_id, token)
        except (HTTPError, URLError) as exc:
            print(f"::warning::edge prune: failed to delete {package} {version}: {exc}", flush=True)
            continue
        removed.append(version)
    return removed


def main() -> None:
    parser = argparse.ArgumentParser(description="Publish packaged fleet artifacts.")
    parser.add_argument(
        "names",
        nargs="+",
        metavar="NAME",
        help="dist/<NAME>/ directories to publish (firmware devices, publisher daemons).",
    )
    parser.add_argument(
        "--kind",
        choices=sorted(KINDS),
        default=os.environ.get("PACKAGE_KIND", DEFAULT_KIND.name),
        help="Payload kind, which selects the OCI artifact type and the flashable guard.",
    )
    parser.add_argument(
        "--dist-root", default="dist", help="Directory containing packaged artifacts."
    )
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
        help=(
            "Validate package namespace access from existing manifests without uploading artifacts."
        ),
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
    kind = resolve_kind(args.kind)
    if args.provider == "ghcr-oci":
        for name in args.names:
            publish_artifact_oci(
                dist_root,
                name,
                args.oci_registry,
                args.oci_owner,
                args.oci_package_prefix,
                args.oci_source_url,
                kind=kind,
            )
            if args.prune_edge:
                manifest = read_manifest(dist_root, name)
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
            raise SystemExit(
                "PACKAGE_AUTH_USER is required when publishing to an org package namespace"
            )
        auth_user = args.package_user
    if (
        package_token
        and args.package_user == PRIVATE_PACKAGE_USER
        and auth_user == args.package_user
    ):
        raise SystemExit("PACKAGE_AUTH_USER must be the PAT-owning user, not stackdrift-firmware")
    if not auth_user:
        auth_user = args.package_user

    for name in args.names:
        if args.preflight_only:
            manifest = read_manifest(dist_root, name)
            preflight_package_access(
                args.base_url,
                auth_user,
                token,
                auth_scheme,
                args.package_user,
                str(manifest["package"]),
            )
            continue
        publish_artifact(
            dist_root,
            name,
            args.package_user,
            auth_user,
            token,
            auth_scheme,
            args.base_url,
            kind=kind,
        )
        if args.prune_edge:
            manifest = read_manifest(dist_root, name)
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

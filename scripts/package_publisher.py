#!/usr/bin/env python3
"""Build and package a Go publisher daemon into the shape publish_packages.py pushes.

Sibling to package_device.py (ESPHome firmware) and build_arduino.py (Arduino
firmware). Those two produce flashable images; this produces a static binary that
runs on a Linux host wired to a sensor, so it publishes under the ``binary``
artifact kind (``application/vnd.stackdrift.grow-publisher.v1``) rather than the
firmware one. Everything else is shared: the ``dist/<name>/`` layout, the
``edge-<UTC>-<sha>`` version scheme, the ``grow-fleet-<package>`` GHCR name, and
the edge-tag pruning.

Pipeline:  go build (CGO_ENABLED=0, cross-compiled to the target GOOS/GOARCH)
           ->  dist/<name>/<name>-<goos>-<goarch> + dist/<name>/<name>.manifest.json
           ->  uv run python scripts/publish_packages.py --kind binary <name>

CGO_ENABLED=0 is a correctness requirement, not a size tweak: the daemon is built
on an x86_64 runner and runs on an arm64 Raspberry Pi, so a cgo build would both
need a cross toolchain and bind the result to the builder's glibc. The checks
workflow asserts the module stays cgo-free by cross-building it the same way.

The manifest deliberately carries no ``flashable`` key. That boolean is what
publish_packages' firmware guard tests, and a binary that is not firmware has no
honest value for it — its absence is what lets ``--kind binary`` accept this
manifest and ``--kind firmware`` refuse it.
"""

from __future__ import annotations

import argparse
import json
import os
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path

from fleetlib import (
    ROOT,
    capture,
    edge_version,
    firmware_channel,
    generated_timestamp,
    md5_file,
    run,
    sha256_file,
)
from publish_packages import BINARY_KIND

PUBLISHERS_DIR = ROOT / "publishers"
MANIFEST_SCHEMA = "grow-publisher-package.v1"


@dataclass(frozen=True)
class PublisherSpec:
    """A Go daemon under ``publishers/`` and the host it is built for."""

    name: str
    module: Path
    main_package: str
    package: str
    package_owner: str
    node_id: str
    goos: str
    goarch: str

    @property
    def binary_name(self) -> str:
        return f"{self.name}-{self.goos}-{self.goarch}"


PUBLISHERS = {
    # quantum-sensor.home.arpa is a Raspberry Pi 5 running 64-bit Raspberry Pi OS,
    # reading an Apogee SQ-521 over SDI-12 through an FTDI FT231X adapter.
    "apogee-sq521": PublisherSpec(
        name="apogee-sq521",
        module=PUBLISHERS_DIR / "apogee-sq521",
        main_package="./cmd/apogee-sq521",
        package="apogee-sq521",
        package_owner="stackdrift",
        node_id="quantum-sensor",
        goos="linux",
        goarch="arm64",
    ),
}


def publisher_spec(name: str) -> PublisherSpec:
    try:
        return PUBLISHERS[name]
    except KeyError:
        known = ", ".join(sorted(PUBLISHERS))
        raise KeyError(f"unknown publisher: {name} (known: {known})") from None


def edge_timestamp() -> str:
    """The ``YYYYMMDDTHHMMSSZ`` stamp firmware.yml builds with ``date -u``."""
    return datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")


def go_version() -> str:
    return capture(["go", "version"])


def build_env(spec: PublisherSpec) -> dict[str, str]:
    return {
        **os.environ,
        "CGO_ENABLED": "0",
        "GOOS": spec.goos,
        "GOARCH": spec.goarch,
    }


def build_binary(spec: PublisherSpec, version: str, dest: Path) -> None:
    """Cross-compile the daemon to ``dest``, which must be an absolute path.

    ``-trimpath`` keeps the runner's checkout directory out of the binary so two
    builds of the same commit agree. ``-X main.version`` is what makes
    ``apogee-sq521 --version`` report the published tag instead of ``dev``; the
    package declares ``var version = "dev"`` for exactly this.
    """
    dest.unlink(missing_ok=True)
    run(
        [
            "go",
            "build",
            "-trimpath",
            "-ldflags",
            f"-s -w -X main.version={version}",
            "-o",
            str(dest),
            spec.main_package,
        ],
        cwd=spec.module,
        env=build_env(spec),
    )
    if not dest.exists():
        raise FileNotFoundError(f"go build produced no binary: {dest}")


def package_publisher(
    name: str,
    version: str,
    source_sha: str,
    dist_root: Path,
    channel: str | None = None,
) -> Path:
    spec = publisher_spec(name)
    # firmware_channel is the fleet-wide version-scheme validator despite the
    # name: it returns "edge"/"stable" and raises on anything that is neither, so
    # a malformed version fails here rather than at the registry.
    resolved_channel = channel or firmware_channel(version)

    dist_dir = dist_root / name
    dist_dir.mkdir(parents=True, exist_ok=True)
    # go build runs with cwd set to the module, so -o has to be absolute or the
    # binary lands under publishers/<name>/dist/ instead of the dist root.
    binary_dest = dist_dir.resolve() / spec.binary_name
    build_binary(spec, version, binary_dest)

    artifact_filenames = [binary_dest.name]
    manifest = {
        "schema": MANIFEST_SCHEMA,
        "kind": BINARY_KIND.name,
        "channel": resolved_channel,
        "publisher": name,
        "node_id": spec.node_id,
        "package_owner": spec.package_owner,
        "package": spec.package,
        "version": version,
        "source_sha": source_sha,
        "goos": spec.goos,
        "goarch": spec.goarch,
        "cgo_enabled": False,
        "go_version": go_version(),
        "generated_at": generated_timestamp(),
        "artifact_filenames": artifact_filenames,
        "md5": {binary_dest.name: md5_file(binary_dest)},
        "sha256": {binary_dest.name: sha256_file(binary_dest)},
    }

    manifest_path = dist_dir / f"{name}.manifest.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest_path


def main() -> None:
    parser = argparse.ArgumentParser(description="Build and package a Go publisher daemon.")
    parser.add_argument("publisher", choices=sorted(PUBLISHERS), help="Publisher to package.")
    parser.add_argument(
        "--version",
        default=None,
        help="Release or build version. Defaults to edge-<now>-<short sha>.",
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
    args = parser.parse_args()

    source_sha = args.source_sha or capture(["git", "rev-parse", "HEAD"])
    version = args.version or edge_version(edge_timestamp(), source_sha)
    manifest_path = package_publisher(args.publisher, version, source_sha, Path(args.dist_root))
    print(manifest_path)


if __name__ == "__main__":
    main()

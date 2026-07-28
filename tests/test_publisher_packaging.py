from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

import publish_packages  # noqa: E402
from firmware_inputs import firmware_impacted_devices  # noqa: E402
from fleetlib import EDGE_VERSION_RE, edge_version  # noqa: E402
from package_publisher import (  # noqa: E402
    MANIFEST_SCHEMA,
    PUBLISHERS,
    build_binary,
    edge_timestamp,
    package_publisher,
    publisher_spec,
)
from publish_packages import (  # noqa: E402
    BINARY_KIND,
    DEFAULT_KIND,
    FIRMWARE_KIND,
    KINDS,
    oci_package_name,
    oci_ref,
    prune_edge_oci_packages,
    publish_artifact_oci,
    resolve_kind,
)

PUBLISHER = "apogee-sq521"
VERSION = "edge-20260620T190102Z-bbbbbbbbbbbb"
SOURCE_SHA = "bbbbbbbbbbbbccccccccccccddddddddddddeeee"
GO_VERSION = "go version go1.25.5 linux/amd64"


def fake_go_build(payload: bytes = b"\x7fELF fake arm64 binary\n"):
    """Stand in for fleetlib.run: honour ``-o`` and write a plausible binary."""

    def build(
        cmd: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None
    ) -> None:
        dest = Path(cmd[cmd.index("-o") + 1])
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(payload)

    return build


def write_manifest(directory: Path, name: str, manifest: dict[str, object]) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / f"{name}.manifest.json"
    path.write_text(json.dumps(manifest), encoding="utf-8")
    return path


def binary_manifest(**overrides: object) -> dict[str, object]:
    manifest: dict[str, object] = {
        "schema": MANIFEST_SCHEMA,
        "kind": "binary",
        "package": PUBLISHER,
        "version": VERSION,
        "artifact_filenames": [f"{PUBLISHER}-linux-arm64"],
    }
    manifest.update(overrides)
    return manifest


class ArtifactKindTests(unittest.TestCase):
    """The kind table is the wire contract; these values are what grow-app matches on."""

    def test_firmware_kind_pins_the_existing_media_types_and_guard(self) -> None:
        self.assertEqual(FIRMWARE_KIND.artifact_type, "application/vnd.stackdrift.grow-firmware.v1")
        self.assertEqual(
            FIRMWARE_KIND.manifest_media_type,
            "application/vnd.stackdrift.grow-firmware.manifest.v1+json",
        )
        self.assertIs(FIRMWARE_KIND.require_flashable, True)

    def test_binary_kind_uses_the_publisher_artifact_type_and_no_flashable_guard(self) -> None:
        self.assertEqual(BINARY_KIND.artifact_type, "application/vnd.stackdrift.grow-publisher.v1")
        self.assertEqual(
            BINARY_KIND.manifest_media_type,
            "application/vnd.stackdrift.grow-publisher.manifest.v1+json",
        )
        self.assertIs(BINARY_KIND.require_flashable, False)

    def test_firmware_stays_the_default_kind(self) -> None:
        self.assertIs(DEFAULT_KIND, FIRMWARE_KIND)
        self.assertEqual(sorted(KINDS), ["binary", "firmware"])

    def test_a_typo_in_package_kind_is_diagnosed_not_a_traceback(self) -> None:
        """argparse validates --kind against `choices`, but not the default it
        reads from PACKAGE_KIND, so the lookup has to diagnose it itself."""
        self.assertIs(resolve_kind("binary"), BINARY_KIND)
        with self.assertRaisesRegex(SystemExit, "unknown artifact kind: nonsense"):
            resolve_kind("nonsense")

    def test_cli_publishes_firmware_when_no_kind_is_given(self) -> None:
        argv = ["publish_packages.py", "atoms3u-sensor-rig"]
        with (
            mock.patch.dict("os.environ", {}, clear=True),
            mock.patch.object(sys, "argv", argv),
            mock.patch("publish_packages.publish_artifact_oci") as publish,
        ):
            publish_packages.main()

        self.assertIs(publish.call_args.kwargs["kind"], FIRMWARE_KIND)


class BinaryKindPublishTests(unittest.TestCase):
    def test_binary_kind_pushes_the_publisher_artifact_type_without_a_flashable_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            artifact_dir = root / PUBLISHER
            write_manifest(artifact_dir, PUBLISHER, binary_manifest())
            (artifact_dir / f"{PUBLISHER}-linux-arm64").write_bytes(b"binary")

            with mock.patch("publish_packages.subprocess.run") as run:
                publish_artifact_oci(
                    root,
                    PUBLISHER,
                    "ghcr.io",
                    "dephekt",
                    "grow-fleet",
                    "https://github.com/dephekt/grow-fleet",
                    kind=BINARY_KIND,
                )

        run.assert_called_once_with(
            [
                "oras",
                "push",
                f"ghcr.io/dephekt/grow-fleet-{PUBLISHER}:{VERSION}",
                "--artifact-type",
                "application/vnd.stackdrift.grow-publisher.v1",
                "--annotation",
                "org.opencontainers.image.source=https://github.com/dephekt/grow-fleet",
                f"{PUBLISHER}-linux-arm64:application/octet-stream",
                f"{PUBLISHER}.manifest.json:"
                "application/vnd.stackdrift.grow-publisher.manifest.v1+json",
            ],
            check=True,
            cwd=artifact_dir,
        )

    def test_binary_kind_refuses_a_firmware_manifest(self) -> None:
        """A firmware payload pushed under the publisher artifact type is invisible
        to grow-app rather than broken, so it has to fail loudly here."""
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_manifest(root / PUBLISHER, PUBLISHER, binary_manifest(flashable=True))

            with (
                self.assertRaisesRegex(ValueError, "refusing to publish a firmware manifest"),
                mock.patch("publish_packages.subprocess.run") as run,
            ):
                publish_artifact_oci(
                    root, PUBLISHER, "ghcr.io", "dephekt", "grow-fleet", kind=BINARY_KIND
                )

        run.assert_not_called()

    def test_firmware_kind_refuses_a_publisher_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_manifest(root / PUBLISHER, PUBLISHER, binary_manifest())

            with (
                self.assertRaisesRegex(ValueError, "refusing to publish non-flashable manifest"),
                mock.patch("publish_packages.subprocess.run") as run,
            ):
                publish_artifact_oci(
                    root, PUBLISHER, "ghcr.io", "dephekt", "grow-fleet", kind=FIRMWARE_KIND
                )

        run.assert_not_called()

    def test_missing_manifest_is_reported_before_anything_is_pushed(self) -> None:
        with (
            tempfile.TemporaryDirectory() as tmp,
            self.assertRaisesRegex(FileNotFoundError, "missing manifest"),
            mock.patch("publish_packages.subprocess.run") as run,
        ):
            publish_artifact_oci(
                Path(tmp), PUBLISHER, "ghcr.io", "dephekt", "grow-fleet", kind=BINARY_KIND
            )

        run.assert_not_called()


class PayloadAgnosticTagTests(unittest.TestCase):
    """The tag/prune layer takes the package name as data; nothing in it is firmware-specific."""

    def test_publisher_reuses_the_firmware_oci_ref_layout(self) -> None:
        self.assertEqual(oci_package_name("grow-fleet", PUBLISHER), f"grow-fleet-{PUBLISHER}")
        self.assertEqual(
            oci_ref("ghcr.io", "dephekt", "grow-fleet", PUBLISHER, VERSION),
            f"ghcr.io/dephekt/grow-fleet-{PUBLISHER}:{VERSION}",
        )

    def test_edge_pruning_works_unchanged_for_a_publisher_package(self) -> None:
        versions = [
            {"id": 7, "metadata": {"container": {"tags": ["edge-20260620T190102Z-bbbbbbbbbbbb"]}}},
            {"id": 8, "metadata": {"container": {"tags": ["edge-20260619T190102Z-cccccccccccc"]}}},
        ]
        with (
            mock.patch.dict("os.environ", {"GHCR_TOKEN": "token"}, clear=False),
            mock.patch(
                "publish_packages.list_oci_tags",
                return_value=[
                    "edge-20260620T190102Z-bbbbbbbbbbbb",
                    "edge-20260619T190102Z-cccccccccccc",
                ],
            ),
            mock.patch("publish_packages.list_ghcr_package_versions", return_value=versions),
            mock.patch("publish_packages.delete_ghcr_package_version") as delete,
        ):
            removed = prune_edge_oci_packages("ghcr.io", "dephekt", "grow-fleet", PUBLISHER, keep=1)

        self.assertEqual(removed, ["edge-20260619T190102Z-cccccccccccc"])
        delete.assert_called_once_with("dephekt", f"grow-fleet-{PUBLISHER}", 8, "token")


class PublisherPackagingTests(unittest.TestCase):
    def test_default_version_matches_the_scheme_firmware_yml_builds_with_date_u(self) -> None:
        stamp = edge_timestamp()
        self.assertRegex(stamp, r"^\d{8}T\d{6}Z$")

        version = edge_version(stamp, SOURCE_SHA)
        self.assertTrue(EDGE_VERSION_RE.fullmatch(version))
        self.assertEqual(version, f"edge-{stamp}-{SOURCE_SHA[:12]}")

    def test_build_is_cgo_free_and_cross_compiled_with_a_stamped_version(self) -> None:
        spec = publisher_spec(PUBLISHER)
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp) / spec.binary_name
            with mock.patch("package_publisher.run", side_effect=fake_go_build()) as run:
                build_binary(spec, VERSION, dest)

        cmd = run.call_args.args[0]
        env = run.call_args.kwargs["env"]
        self.assertEqual(cmd[:2], ["go", "build"])
        self.assertIn("-trimpath", cmd)
        self.assertEqual(cmd[cmd.index("-ldflags") + 1], f"-s -w -X main.version={VERSION}")
        self.assertEqual(cmd[-1], "./cmd/apogee-sq521")
        self.assertEqual(env["CGO_ENABLED"], "0")
        self.assertEqual(env["GOOS"], "linux")
        self.assertEqual(env["GOARCH"], "arm64")
        self.assertEqual(run.call_args.kwargs["cwd"], spec.module)

    def test_build_writes_an_absolute_destination_under_the_dist_root(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            dist = Path(tmp) / "dist"
            with (
                mock.patch("package_publisher.run", side_effect=fake_go_build()) as run,
                mock.patch("package_publisher.go_version", return_value=GO_VERSION),
            ):
                package_publisher(PUBLISHER, VERSION, SOURCE_SHA, dist)

            dest = Path(run.call_args.args[0][run.call_args.args[0].index("-o") + 1])
            self.assertTrue(dest.is_absolute())
            self.assertEqual(dest, (dist / PUBLISHER).resolve() / f"{PUBLISHER}-linux-arm64")
            self.assertTrue(dest.exists())

    def test_build_failure_leaves_no_stale_binary_to_republish(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            dist = Path(tmp) / "dist"
            with (
                mock.patch("package_publisher.run", side_effect=fake_go_build()),
                mock.patch("package_publisher.go_version", return_value=GO_VERSION),
            ):
                package_publisher(PUBLISHER, VERSION, SOURCE_SHA, dist)

            binary = dist / PUBLISHER / f"{PUBLISHER}-linux-arm64"
            self.assertTrue(binary.exists())

            with (
                mock.patch("package_publisher.run"),  # a "successful" build that writes nothing
                mock.patch("package_publisher.go_version", return_value=GO_VERSION),
                self.assertRaisesRegex(FileNotFoundError, "go build produced no binary"),
            ):
                package_publisher(PUBLISHER, VERSION, SOURCE_SHA, dist)

            self.assertFalse(binary.exists())

    def test_manifest_carries_the_binary_shape_and_no_flashable_key(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            dist = Path(tmp) / "dist"
            with (
                mock.patch("package_publisher.run", side_effect=fake_go_build(b"payload")),
                mock.patch("package_publisher.go_version", return_value=GO_VERSION),
            ):
                manifest_path = package_publisher(PUBLISHER, VERSION, SOURCE_SHA, dist)

            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

        self.assertEqual(manifest_path, dist / PUBLISHER / f"{PUBLISHER}.manifest.json")
        self.assertEqual(manifest["schema"], MANIFEST_SCHEMA)
        self.assertEqual(manifest["kind"], BINARY_KIND.name)
        self.assertEqual(manifest["channel"], "edge")
        self.assertEqual(manifest["package"], PUBLISHER)
        self.assertEqual(manifest["version"], VERSION)
        self.assertEqual(manifest["source_sha"], SOURCE_SHA)
        self.assertEqual(manifest["node_id"], "quantum-sensor")
        self.assertEqual(manifest["goos"], "linux")
        self.assertEqual(manifest["goarch"], "arm64")
        self.assertIs(manifest["cgo_enabled"], False)
        self.assertEqual(manifest["go_version"], GO_VERSION)
        self.assertEqual(manifest["artifact_filenames"], [f"{PUBLISHER}-linux-arm64"])
        self.assertEqual(
            manifest["sha256"][f"{PUBLISHER}-linux-arm64"],
            "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5",
        )
        self.assertNotIn("flashable", manifest)

    def test_manifest_rejects_a_version_outside_the_fleet_scheme(self) -> None:
        with (
            tempfile.TemporaryDirectory() as tmp,
            mock.patch("package_publisher.run", side_effect=fake_go_build()) as run,
            self.assertRaisesRegex(ValueError, "unsupported firmware version format"),
        ):
            package_publisher(PUBLISHER, "v0.1-not-semver", SOURCE_SHA, Path(tmp))

        run.assert_not_called()

    def test_unknown_publisher_names_the_known_ones(self) -> None:
        with self.assertRaisesRegex(KeyError, "unknown publisher: nope"):
            publisher_spec("nope")

    def test_packaged_publisher_is_publishable_under_the_binary_kind(self) -> None:
        """The two halves have to agree: what package_publisher writes is exactly
        what publish_packages --kind binary accepts."""
        with tempfile.TemporaryDirectory() as tmp:
            dist = Path(tmp) / "dist"
            with (
                mock.patch("package_publisher.run", side_effect=fake_go_build()),
                mock.patch("package_publisher.go_version", return_value=GO_VERSION),
            ):
                package_publisher(PUBLISHER, VERSION, SOURCE_SHA, dist)

            with mock.patch("publish_packages.subprocess.run") as run:
                publish_artifact_oci(
                    dist, PUBLISHER, "ghcr.io", "dephekt", "grow-fleet", kind=BINARY_KIND
                )

        args = run.call_args.args[0]
        self.assertEqual(args[2], f"ghcr.io/dephekt/grow-fleet-{PUBLISHER}:{VERSION}")
        self.assertEqual(args[4], BINARY_KIND.artifact_type)
        self.assertTrue(args[-1].endswith(BINARY_KIND.manifest_media_type))

    def test_apogee_publisher_points_at_the_committed_go_module(self) -> None:
        spec = PUBLISHERS[PUBLISHER]
        self.assertTrue((spec.module / "go.mod").exists())
        self.assertTrue((spec.module / "cmd" / PUBLISHER / "main.go").exists())

    def test_publisher_changes_do_not_fan_out_into_firmware_rebuilds(self) -> None:
        """The daemon shares scripts/ with the firmware pipeline, and
        firmware_impacted_devices treats only an allowlist of scripts as firmware
        inputs. Nothing added for the publisher may join it, or every merge would
        recompile and republish the whole ESPHome release fleet."""
        self.assertEqual(
            firmware_impacted_devices(
                [
                    "publishers/apogee-sq521/internal/app/poll.go",
                    "publishers/apogee-sq521/go.mod",
                    "scripts/package_publisher.py",
                    "tests/test_publisher_packaging.py",
                    ".github/workflows/publisher.yml",
                ]
            ),
            [],
        )


if __name__ == "__main__":
    unittest.main()

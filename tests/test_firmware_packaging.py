from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import (  # noqa: E402
    assert_flashable_secrets,
    device_names,
    device_spec,
    edge_version,
    esphome_command,
    firmware_channel,
    flashable_secret_problems,
    md5_file,
    sha256_file,
    stable_version_key,
)
from compile_devices import compile_device  # noqa: E402
from edge_changelog_base import download_oci_manifest, latest_edge_package  # noqa: E402
from edge_build_devices import edge_build_devices, should_build_device  # noqa: E402
from firmware_inputs import firmware_impacted_devices  # noqa: E402
from package_device import latest_stable_tag, package_device, previous_stable_tag, release_metadata  # noqa: E402
from publish_packages import (  # noqa: E402
    OCI_ARTIFACT_TYPE,
    OCI_MANIFEST_MEDIA_TYPE,
    edge_cleanup_candidates,
    list_generic_packages,
    oci_package_name,
    oci_ref,
    publish_device_oci,
    prune_edge_oci_packages,
)


class FirmwarePackagingTests(unittest.TestCase):
    def test_channel_parsing_accepts_stable_and_edge_versions(self) -> None:
        self.assertEqual(stable_version_key("v1.2.3"), (1, 2, 3))
        self.assertEqual(firmware_channel("v1.2.3"), "stable")
        self.assertEqual(firmware_channel("edge-20260620T180102Z-012345abcdef"), "edge")
        self.assertEqual(edge_version("20260620T180102Z", "012345abcdef9876"), "edge-20260620T180102Z-012345abcdef")

    def test_channel_parsing_rejects_non_release_versions(self) -> None:
        with self.assertRaises(ValueError):
            firmware_channel("v0.1-cache-test")
        with self.assertRaises(ValueError):
            edge_version("2026-06-20T18:01:02Z", "012345abcdef9876")

    def test_stable_changelog_tag_selection_ignores_other_devices_and_non_semver_tags(self) -> None:
        tags = [
            "firmware/atoms3u-sensor-rig/v0.1-cache-test",
            "firmware/atoms3u-sensor-rig/v0.1.0",
            "firmware/atoms3u-sensor-rig/v0.2.0",
            "firmware/m5stack-airq/v9.9.9",
        ]

        self.assertEqual(previous_stable_tag(tags, "atoms3u-sensor-rig", "v0.2.0"), "firmware/atoms3u-sensor-rig/v0.1.0")
        self.assertIsNone(previous_stable_tag(tags, "atoms3u-sensor-rig", "v0.1.0"))
        self.assertEqual(latest_stable_tag(tags, "atoms3u-sensor-rig"), "firmware/atoms3u-sensor-rig/v0.2.0")

    def test_github_workflow_changes_do_not_impact_firmware_devices(self) -> None:
        self.assertEqual(firmware_impacted_devices([".github/workflows/firmware.yml"]), [])

    def test_package_only_script_changes_do_not_impact_firmware_devices(self) -> None:
        self.assertEqual(
            firmware_impacted_devices(
                [
                    "scripts/edge_changelog_base.py",
                    "scripts/publish_packages.py",
                    "tests/test_firmware_packaging.py",
                    "README.md",
                ]
            ),
            [],
        )

    def test_compile_script_changes_impact_all_release_devices(self) -> None:
        self.assertEqual(firmware_impacted_devices(["scripts/compile_devices.py"]), device_names(release_only=True))

    def test_device_asset_change_impacts_asset_owner(self) -> None:
        self.assertEqual(firmware_impacted_devices(["assets/thermal_overlay.js"]), ["atoms3u-sensor-rig"])

    def test_airq_is_available_for_explicit_builds_but_not_release_selection(self) -> None:
        self.assertIn("m5stack-airq", device_names())
        self.assertNotIn("m5stack-airq", device_names(release_only=True))

    def test_airq_config_change_does_not_trigger_release_builds(self) -> None:
        self.assertEqual(firmware_impacted_devices(["devices/m5stack-airq.yaml"]), [])

    def test_esphome_command_defaults_to_esphome(self) -> None:
        with mock.patch.dict("os.environ", {}, clear=True):
            self.assertEqual(esphome_command(), ["esphome"])

    def test_esphome_command_uses_environment_override(self) -> None:
        with mock.patch.dict("os.environ", {"ESPHOME": "./docker/esphome --verbose"}):
            self.assertEqual(esphome_command(), ["./docker/esphome", "--verbose"])

    def test_compile_device_uses_esphome_override_and_relative_config_path(self) -> None:
        with (
            mock.patch.dict("os.environ", {"ESPHOME": "./docker/esphome"}),
            mock.patch("compile_devices.ensure_secrets_link") as ensure_secrets_link,
            mock.patch("compile_devices.run") as run,
        ):
            compile_device("atoms3u-sensor-rig")

        ensure_secrets_link.assert_called_once_with()
        run.assert_called_once_with(
            [
                "./docker/esphome",
                "-s",
                "package_owner",
                "stackdrift-firmware",
                "compile",
                "devices/atoms3u-sensor-rig.yaml",
            ]
        )

    def test_edge_release_metadata_uses_previous_edge_base_when_provided(self) -> None:
        commits = [{"sha": "cccccccccccc", "subject": "new edge change"}]
        with (
            mock.patch("package_device.git_tags", return_value=["firmware/atoms3u-sensor-rig/v0.1.0"]),
            mock.patch("package_device.git_commits", return_value=commits) as git_commits,
        ):
            metadata = release_metadata(
                "atoms3u-sensor-rig",
                "edge",
                "edge-20260620T190102Z-cccccccccccc",
                "ccccccccccccdddddddddddddddddddddddddddd",
                changelog_base_ref="bbbbbbbbbbbbaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                changelog_base_version="edge-20260620T180102Z-bbbbbbbbbbbb",
            )

        git_commits.assert_called_once_with("bbbbbbbbbbbbaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ccccccccccccdddddddddddddddddddddddddddd")
        self.assertEqual(metadata["release_summary"], "1 commits since edge-20260620T180102Z-bbbbbbbbbbbb")
        self.assertEqual(metadata["changelog"]["base_ref"], "bbbbbbbbbbbbaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
        self.assertIsNone(metadata["changelog"]["base_tag"])
        self.assertEqual(metadata["changelog"]["base_version"], "edge-20260620T180102Z-bbbbbbbbbbbb")
        self.assertEqual(metadata["changelog"]["commits"], commits)

    def test_edge_release_metadata_without_previous_edge_does_not_use_stable_tag(self) -> None:
        with (
            mock.patch("package_device.git_tags", return_value=["firmware/atoms3u-sensor-rig/v0.1.0"]),
            mock.patch("package_device.git_commits", return_value=[]) as git_commits,
        ):
            metadata = release_metadata(
                "atoms3u-sensor-rig",
                "edge",
                "edge-20260620T180102Z-aaaaaaaaaaaa",
                "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd",
            )

        git_commits.assert_called_once_with(None, "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd")
        self.assertEqual(metadata["release_summary"], "Initial edge firmware package for atoms3u-sensor-rig")
        self.assertIsNone(metadata["changelog"]["base_ref"])
        self.assertIsNone(metadata["changelog"]["base_tag"])
        self.assertIsNone(metadata["changelog"]["base_version"])

    def test_latest_edge_package_ignores_non_edge_and_excluded_versions(self) -> None:
        packages = [
            {"name": "atoms3u-sensor-rig", "version": "v0.1.0"},
            {"name": "atoms3u-sensor-rig", "version": "edge-20260620T180102Z-aaaaaaaaaaaa"},
            {"name": "atoms3u-sensor-rig", "version": "edge-20260620T190102Z-bbbbbbbbbbbb"},
            {"name": "atoms3u-sensor-rig", "version": "edge-20260620T200102Z-cccccccccccc"},
        ]

        self.assertEqual(
            latest_edge_package(packages, exclude_version="edge-20260620T200102Z-cccccccccccc"),
            {"name": "atoms3u-sensor-rig", "version": "edge-20260620T190102Z-bbbbbbbbbbbb"},
        )

    def test_download_oci_manifest_suppresses_oras_progress_stdout(self) -> None:
        def run_oras(cmd: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            output_dir = Path(cmd[cmd.index("--output") + 1])
            (output_dir / "atoms3u-sensor-rig.manifest.json").write_text(
                json.dumps({"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}),
                encoding="utf-8",
            )
            return subprocess.CompletedProcess(cmd, 0)

        with mock.patch("edge_changelog_base.subprocess.run", side_effect=run_oras) as run:
            manifest = download_oci_manifest(
                "ghcr.io",
                "dephekt",
                "grow-fleet",
                "atoms3u-sensor-rig",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
                "atoms3u-sensor-rig.manifest.json",
            )

        self.assertEqual(manifest, {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"})
        run.assert_called_once()
        self.assertIs(run.call_args.kwargs["stdout"], subprocess.DEVNULL)

    def test_download_oci_manifest_finds_nested_oras_artifact_path(self) -> None:
        def run_oras(cmd: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            output_dir = Path(cmd[cmd.index("--output") + 1])
            manifest_path = output_dir / "dist" / "atoms3u-sensor-rig" / "atoms3u-sensor-rig.manifest.json"
            manifest_path.parent.mkdir(parents=True)
            manifest_path.write_text(
                json.dumps({"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}),
                encoding="utf-8",
            )
            return subprocess.CompletedProcess(cmd, 0)

        with mock.patch("edge_changelog_base.subprocess.run", side_effect=run_oras):
            manifest = download_oci_manifest(
                "ghcr.io",
                "dephekt",
                "grow-fleet",
                "atoms3u-sensor-rig",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
                "atoms3u-sensor-rig.manifest.json",
            )

        self.assertEqual(manifest, {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"})

    def test_download_oci_manifest_reports_restored_files_when_missing(self) -> None:
        def run_oras(cmd: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            output_dir = Path(cmd[cmd.index("--output") + 1])
            artifact_path = output_dir / "dist" / "atoms3u-sensor-rig" / "atoms3u-sensor-rig.ota.bin"
            artifact_path.parent.mkdir(parents=True)
            artifact_path.write_bytes(b"ota")
            return subprocess.CompletedProcess(cmd, 0)

        with (
            mock.patch("edge_changelog_base.subprocess.run", side_effect=run_oras),
            self.assertRaisesRegex(FileNotFoundError, "dist/atoms3u-sensor-rig/atoms3u-sensor-rig.ota.bin"),
        ):
            download_oci_manifest(
                "ghcr.io",
                "dephekt",
                "grow-fleet",
                "atoms3u-sensor-rig",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
                "atoms3u-sensor-rig.manifest.json",
            )

    def test_edge_cleanup_keeps_newest_versions(self) -> None:
        versions = [
            "v0.1.0",
            "edge-20260620T180102Z-aaaaaaaaaaaa",
            "edge-20260620T190102Z-bbbbbbbbbbbb",
            "edge-20260619T190102Z-cccccccccccc",
        ]

        self.assertEqual(
            edge_cleanup_candidates(versions, keep=2),
            ["edge-20260619T190102Z-cccccccccccc"],
        )

    def test_oci_reference_uses_prefixed_per_device_package(self) -> None:
        self.assertEqual(
            oci_package_name("grow-fleet", "atoms3u-sensor-rig"),
            "grow-fleet-atoms3u-sensor-rig",
        )
        self.assertEqual(
            oci_ref(
                "ghcr.io",
                "dephekt",
                "grow-fleet",
                "atoms3u-sensor-rig",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
            ),
            "ghcr.io/dephekt/grow-fleet-atoms3u-sensor-rig:edge-20260620T190102Z-bbbbbbbbbbbb",
        )

    def test_publish_device_oci_pushes_flashable_manifest_and_artifacts_without_source_annotation_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            device_dir = root / "atoms3u-sensor-rig"
            device_dir.mkdir()
            (device_dir / "atoms3u-sensor-rig.ota.bin").write_bytes(b"ota")
            (device_dir / "atoms3u-sensor-rig.factory.bin").write_bytes(b"factory")
            manifest_path = device_dir / "atoms3u-sensor-rig.manifest.json"
            manifest_path.write_text(
                json.dumps(
                    {
                        "flashable": True,
                        "package": "atoms3u-sensor-rig",
                        "version": "edge-20260620T190102Z-bbbbbbbbbbbb",
                        "artifact_filenames": [
                            "atoms3u-sensor-rig.ota.bin",
                            "atoms3u-sensor-rig.factory.bin",
                        ],
                    }
                ),
                encoding="utf-8",
            )

            with mock.patch("publish_packages.subprocess.run") as run:
                publish_device_oci(
                    root,
                    "atoms3u-sensor-rig",
                    "ghcr.io",
                    "dephekt",
                    "grow-fleet",
                )

        run.assert_called_once_with(
            [
                "oras",
                "push",
                "ghcr.io/dephekt/grow-fleet-atoms3u-sensor-rig:edge-20260620T190102Z-bbbbbbbbbbbb",
                "--artifact-type",
                OCI_ARTIFACT_TYPE,
                "atoms3u-sensor-rig.ota.bin:application/octet-stream",
                "atoms3u-sensor-rig.factory.bin:application/octet-stream",
                f"atoms3u-sensor-rig.manifest.json:{OCI_MANIFEST_MEDIA_TYPE}",
            ],
            check=True,
            cwd=device_dir,
        )

    def test_publish_device_oci_can_attach_source_annotation_when_explicit(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            device_dir = root / "atoms3u-sensor-rig"
            device_dir.mkdir()
            (device_dir / "atoms3u-sensor-rig.ota.bin").write_bytes(b"ota")
            (device_dir / "atoms3u-sensor-rig.factory.bin").write_bytes(b"factory")
            manifest_path = device_dir / "atoms3u-sensor-rig.manifest.json"
            manifest_path.write_text(
                json.dumps(
                    {
                        "flashable": True,
                        "package": "atoms3u-sensor-rig",
                        "version": "edge-20260620T190102Z-bbbbbbbbbbbb",
                        "artifact_filenames": [
                            "atoms3u-sensor-rig.ota.bin",
                            "atoms3u-sensor-rig.factory.bin",
                        ],
                    }
                ),
                encoding="utf-8",
            )

            with mock.patch("publish_packages.subprocess.run") as run:
                publish_device_oci(
                    root,
                    "atoms3u-sensor-rig",
                    "ghcr.io",
                    "dephekt",
                    "grow-fleet",
                    "https://github.com/dephekt/grow-fleet",
                )

        run.assert_called_once_with(
            [
                "oras",
                "push",
                "ghcr.io/dephekt/grow-fleet-atoms3u-sensor-rig:edge-20260620T190102Z-bbbbbbbbbbbb",
                "--artifact-type",
                OCI_ARTIFACT_TYPE,
                "--annotation",
                "org.opencontainers.image.source=https://github.com/dephekt/grow-fleet",
                "atoms3u-sensor-rig.ota.bin:application/octet-stream",
                "atoms3u-sensor-rig.factory.bin:application/octet-stream",
                f"atoms3u-sensor-rig.manifest.json:{OCI_MANIFEST_MEDIA_TYPE}",
            ],
            check=True,
            cwd=device_dir,
        )

    def test_publish_device_oci_rejects_non_flashable_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            device_dir = root / "atoms3u-sensor-rig"
            device_dir.mkdir()
            (device_dir / "atoms3u-sensor-rig.manifest.json").write_text(
                json.dumps(
                    {
                        "flashable": False,
                        "package": "atoms3u-sensor-rig",
                        "version": "edge-20260620T190102Z-bbbbbbbbbbbb",
                        "artifact_filenames": [],
                    }
                ),
                encoding="utf-8",
            )

            with self.assertRaises(ValueError), mock.patch("publish_packages.subprocess.run") as run:
                publish_device_oci(root, "atoms3u-sensor-rig", "ghcr.io", "dephekt", "grow-fleet")

        run.assert_not_called()

    def test_prune_edge_oci_packages_deletes_old_edge_tags(self) -> None:
        with (
            mock.patch(
                "publish_packages.list_oci_tags",
                return_value=[
                    "v0.1.0",
                    "edge-20260620T180102Z-aaaaaaaaaaaa",
                    "edge-20260620T190102Z-bbbbbbbbbbbb",
                    "edge-20260619T190102Z-cccccccccccc",
                ],
            ),
            mock.patch("publish_packages.subprocess.run") as run,
        ):
            removed = prune_edge_oci_packages("ghcr.io", "dephekt", "grow-fleet", "atoms3u-sensor-rig", keep=2)

        self.assertEqual(removed, ["edge-20260619T190102Z-cccccccccccc"])
        run.assert_called_once_with(
            [
                "oras",
                "manifest",
                "delete",
                "--force",
                "ghcr.io/dephekt/grow-fleet-atoms3u-sensor-rig:edge-20260619T190102Z-cccccccccccc",
            ],
            check=True,
        )

    def test_edge_build_devices_selects_devices_without_previous_package(self) -> None:
        with mock.patch("edge_build_devices.latest_edge_manifest", return_value=None):
            self.assertTrue(should_build_device("atoms3u-sensor-rig", "HEAD"))

    def test_edge_build_devices_selects_changed_device_since_previous_edge(self) -> None:
        manifest = {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}
        with (
            mock.patch("edge_build_devices.latest_edge_manifest", return_value=manifest),
            mock.patch("edge_build_devices.commit_exists", return_value=True),
            mock.patch("edge_build_devices.changed_paths", return_value=["devices/atoms3u-sensor-rig.yaml"]) as changed,
        ):
            self.assertTrue(should_build_device("atoms3u-sensor-rig", "HEAD"))
            self.assertFalse(should_build_device("atlas-hydro-kit", "HEAD"))

        changed.assert_called_with("aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd", "HEAD")

    def test_edge_build_devices_skips_workflow_only_changes(self) -> None:
        manifest = {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}
        with (
            mock.patch("edge_build_devices.latest_edge_manifest", return_value=manifest),
            mock.patch("edge_build_devices.commit_exists", return_value=True),
            mock.patch("edge_build_devices.changed_paths", return_value=[".github/workflows/firmware.yml"]),
        ):
            self.assertEqual(edge_build_devices("HEAD"), [])

    def test_edge_build_devices_skips_unrelated_changes(self) -> None:
        manifest = {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}
        with (
            mock.patch("edge_build_devices.latest_edge_manifest", return_value=manifest),
            mock.patch("edge_build_devices.commit_exists", return_value=True),
            mock.patch("edge_build_devices.changed_paths", return_value=["README.md"]),
        ):
            self.assertEqual(edge_build_devices("HEAD"), [])

    def test_edge_build_devices_skips_package_only_script_changes(self) -> None:
        manifest = {"source_sha": "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd"}
        with (
            mock.patch("edge_build_devices.latest_edge_manifest", return_value=manifest),
            mock.patch("edge_build_devices.commit_exists", return_value=True),
            mock.patch("edge_build_devices.changed_paths", return_value=["scripts/publish_packages.py"]),
        ):
            self.assertEqual(edge_build_devices("HEAD"), [])

    def test_edge_build_devices_is_conservative_for_unreadable_manifest(self) -> None:
        with mock.patch("edge_build_devices.latest_edge_manifest", side_effect=RuntimeError("denied")):
            self.assertTrue(should_build_device("atoms3u-sensor-rig", "HEAD"))

    def test_package_listing_reads_all_pages(self) -> None:
        calls: list[str] = []

        class Response:
            def __init__(self, payload: list[dict[str, object]], link: str | None = None) -> None:
                self.payload = payload
                self.headers = {"Link": link} if link else {}

            def __enter__(self) -> "Response":
                return self

            def __exit__(self, *_: object) -> None:
                return None

            def read(self) -> bytes:
                return json_bytes(self.payload)

        def fake_urlopen(request: object) -> Response:
            url = request.full_url  # type: ignore[attr-defined]
            self.assertEqual(request.headers.get("Authorization"), "Basic c3RhY2tkcmlmdDp0b2tlbg==")  # type: ignore[attr-defined]
            calls.append(url)
            if "page=1" in url:
                return Response(
                    [
                        {"name": "atlas-hydro-kit", "version": "edge-20260620T180102Z-aaaaaaaaaaaa"},
                        {"name": "other-device", "version": "v9.9.9"},
                    ],
                    (
                        "<https://codeberg.org/api/v1/packages/stackdrift"
                        "?limit=50&page=2&q=atlas-hydro-kit&type=generic>; rel=\"next\""
                    ),
                )
            return Response(
                [
                    {
                        "name": "atlas-hydro-kit",
                        "version": "edge-20260620T190102Z-bbbbbbbbbbbb",
                    }
                ]
            )

        with mock.patch("publish_packages.urlopen", side_effect=fake_urlopen):
            packages = list_generic_packages(
                "https://codeberg.org",
                "stackdrift-firmware",
                "atlas-hydro-kit",
                auth_user="stackdrift",
                token="token",
            )

        self.assertEqual(
            [package["version"] for package in packages],
            [
                "edge-20260620T180102Z-aaaaaaaaaaaa",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
            ],
        )
        self.assertEqual([url.split("page=", 1)[1].split("&", 1)[0] for url in calls], ["1", "2"])
        self.assertTrue(all("limit=50" in url for url in calls))

    def test_fleet_uses_private_firmware_package_owner(self) -> None:
        self.assertEqual(device_spec("atoms3u-sensor-rig").package_owner, "stackdrift-firmware")

    def test_flashable_secret_guard_rejects_placeholders(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            placeholder = root / "ci-secrets.yaml"
            candidate = root / "devices-secrets.yaml"
            placeholder.write_text(
                "\n".join(
                    [
                        'api_encryption_key: "compile-only-api"',
                        'fallback_hotspot_key: "compile-only-hotspot"',
                        'firmware_update_token: "compile-only-token"',
                        'hydro_monitor_ota_key: "compile-only-ota"',
                        'mqtt_password: "compile-only-mqtt"',
                        'wifi_password: "compile-only-wifi-password"',
                        'wifi_ssid: "compile-only-wifi-ssid"',
                    ]
                ),
                encoding="utf-8",
            )
            candidate.write_text(placeholder.read_text(encoding="utf-8"), encoding="utf-8")

            problems = flashable_secret_problems(candidate, placeholder)

        self.assertIn("secret still has compile-only placeholder value: firmware_update_token", problems)

    def test_flashable_secret_guard_accepts_real_values(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            placeholder = root / "ci-secrets.yaml"
            candidate = root / "devices-secrets.yaml"
            placeholder.write_text(
                "\n".join(
                    [
                        'api_encryption_key: "compile-only-api"',
                        'fallback_hotspot_key: "compile-only-hotspot"',
                        'firmware_update_token: "compile-only-token"',
                        'hydro_monitor_ota_key: "compile-only-ota"',
                        'mqtt_password: "compile-only-mqtt"',
                        'wifi_password: "compile-only-wifi-password"',
                        'wifi_ssid: "compile-only-wifi-ssid"',
                    ]
                ),
                encoding="utf-8",
            )
            candidate.write_text(
                "\n".join(
                    [
                        'api_encryption_key: "real-api"',
                        'fallback_hotspot_key: "real-hotspot"',
                        'firmware_update_token: "real-token"',
                        'hydro_monitor_ota_key: "real-ota"',
                        'mqtt_password: "real-mqtt"',
                        'wifi_password: "real-wifi-password"',
                        'wifi_ssid: "real-wifi-ssid"',
                    ]
                ),
                encoding="utf-8",
            )

            assert_flashable_secrets(candidate, placeholder)

    def test_package_manifest_marks_private_packages_flashable(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            ota = root / "firmware.ota.bin"
            factory = root / "firmware.factory.bin"
            dist = root / "dist"
            ota.write_bytes(b"ota")
            factory.write_bytes(b"factory")

            with (
                mock.patch("package_device.firmware_artifacts", return_value={"ota": ota, "factory": factory}),
                mock.patch("package_device.esphome_version", return_value="ESPHome 2026.5.1"),
                mock.patch("package_device.git_tags", return_value=[]),
                mock.patch("package_device.git_commits", return_value=[]),
            ):
                manifest_path = package_device(
                    "atoms3u-sensor-rig",
                    "edge-20260620T180102Z-aaaaaaaaaaaa",
                    "aaaaaaaaaaaabbbbbbbbbbbbccccccccccccdddd",
                    dist,
                    channel="edge",
                )

            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

        self.assertEqual(manifest["package_owner"], "stackdrift-firmware")
        self.assertEqual(manifest["build_profile"], "site-private")
        self.assertIs(manifest["flashable"], True)

    def test_checksum_helpers_hash_artifacts(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            artifact = Path(tmp) / "firmware.ota.bin"
            artifact.write_bytes(b"grow firmware\n")

            self.assertEqual(md5_file(artifact), "4a3b8aa1363813d51abb788cfd4c294e")
            self.assertEqual(sha256_file(artifact), "7711f755d25874261ba889d6c343474b3952fd5f90d8918833d2e375bf8468c2")


def json_bytes(payload: object) -> bytes:
    return json.dumps(payload).encode("utf-8")


if __name__ == "__main__":
    unittest.main()

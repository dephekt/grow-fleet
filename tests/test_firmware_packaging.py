from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import (  # noqa: E402
    assert_flashable_secrets,
    device_spec,
    edge_version,
    firmware_channel,
    flashable_secret_problems,
    md5_file,
    sha256_file,
    stable_version_key,
)
from package_device import latest_stable_tag, package_device, previous_stable_tag  # noqa: E402
from publish_packages import edge_cleanup_candidates, list_generic_packages  # noqa: E402


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

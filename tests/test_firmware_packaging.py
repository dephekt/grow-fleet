from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import edge_version, firmware_channel, md5_file, sha256_file, stable_version_key  # noqa: E402
from package_device import latest_stable_tag, previous_stable_tag  # noqa: E402
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
            packages = list_generic_packages("https://codeberg.org", "stackdrift", "atlas-hydro-kit")

        self.assertEqual(
            [package["version"] for package in packages],
            [
                "edge-20260620T180102Z-aaaaaaaaaaaa",
                "edge-20260620T190102Z-bbbbbbbbbbbb",
            ],
        )
        self.assertEqual([url.split("page=", 1)[1].split("&", 1)[0] for url in calls], ["1", "2"])
        self.assertTrue(all("limit=50" in url for url in calls))

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

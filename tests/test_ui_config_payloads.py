# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Guards the `_ui/config` retained payload against the ESP8266 MQTT publish ceiling.

AsyncMqttClient refuses any publish whose whole packet exceeds the TCP send
buffer (`if (_client.space() < neededSpace) return 0;`), which on ESP8266 is
2 x TCP_MSS = 2920 bytes. ESPHome retries once immediately -- into the same full
buffer -- then drops the message, and the plug baseline logs nothing (serial
`baud_rate: 0`, MQTT `log_topic` level NONE). The device therefore looks
healthy, publishes every entity, and is simply missing from grow-app's
dashboard. Only a size check catches it before the flash.

The publish is located structurally rather than by scraping the literal
`payload: |-` block, so a device is free to change block-scalar style without
silently falling out of the guard.
"""

from __future__ import annotations

import json
import re
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import DeviceSpec, _load_device_config, iter_device_specs  # noqa: E402

# 2 x TCP_MSS on ESP8266.
ESP8266_TCP_SND_BUF = 2920

UI_CONFIG_SUFFIX = "/_ui/config"

# The whole fleet emits v2 -- v1 with short keys and defaulted values omitted.
# grow-app still accepts v1, because a device only changes encoding when it is
# reflashed, but nothing in this repo should declare v1 again.
UI_SCHEMA = "grow-ui.v2"

# Canonical key legend. grow-app's ui-metadata.ts is the parser, but it lives in
# another repo, so anyone hand-editing a payload here reads this instead.
#
#   groups     i -> id       t -> title    n -> order   v -> variant
#              s -> surface  d -> deviceSettingsSection p -> defaultOpen
#   entities   c -> component  o -> objectId  g -> group
#              n -> order      r -> role      l -> label
#
# Values the parser already defaults are omitted rather than sent: defaultOpen
# false, and any absent role/label/variant/surface/deviceSettingsSection.
V2_GROUP_KEYS = {"i", "t", "n", "v", "s", "d", "p"}
V2_ENTITY_KEYS = {"c", "o", "g", "n", "r", "l"}

# Device sources that declare a UI schema, including the non-ESPHome ones.
# iter_device_specs() reads only fleet.yaml's `devices:` map, so the Arduino
# nodes under `arduino_devices:` are invisible to every structural check below --
# which is exactly how the spectrometer sat on v1 unnoticed.
UI_SOURCE_GLOBS = ("devices/*.yaml", "arduino/*/*.ino")

# The JSON schema field itself, in both the plain YAML spelling and the
# backslash-escaped one a C string literal uses. Matching bare "grow-ui.vN"
# anywhere in the text would also flag prose that merely mentions the old
# encoding, so the version is read from the declaration rather than the file.
SCHEMA_DECLARATION = re.compile(r'schema\\?"\s*:\s*\\?"(grow-ui\.v\d+)')


def _substitute(text: str, substitutions: dict[str, object]) -> str:
    for key, value in substitutions.items():
        text = text.replace(f"${{{key}}}", str(value))
    return text


def ui_config_publish(spec: DeviceSpec) -> tuple[str, str] | None:
    """The device's `_ui/config` (topic, payload) with substitutions applied.

    None means the device declares no such publish at all. A declared publish
    that cannot be read raises, because a guard that quietly finds nothing is
    the failure mode this module exists to prevent.
    """
    config = _load_device_config(spec.config)
    substitutions = config.get("substitutions") or {}
    mqtt = config.get("mqtt")
    if not isinstance(mqtt, dict):
        return None

    for action in mqtt.get("on_connect") or []:
        if not isinstance(action, dict):
            continue
        publish = action.get("mqtt.publish")
        if not isinstance(publish, dict):
            continue
        topic = _substitute(str(publish.get("topic", "")), substitutions)
        if not topic.endswith(UI_CONFIG_SUFFIX):
            continue
        payload = publish.get("payload")
        if not isinstance(payload, str):
            raise AssertionError(
                f"{spec.name} declares {topic} but its payload did not parse as a "
                f"string (got {type(payload).__name__}); the size guard cannot measure it"
            )
        return topic, _substitute(payload, substitutions)
    return None


def packet_size(topic: str, payload: str) -> int:
    """Bytes AsyncMqttClient must fit in one shot: header, topic, packet id, payload."""
    fixed_header = 1
    remaining_length = 2  # two varint bytes covers every payload we publish
    topic_length_prefix = 2
    packet_id = 2  # qos 1
    return (
        fixed_header
        + remaining_length
        + topic_length_prefix
        + len(topic)
        + packet_id
        + len(payload)
    )


def esp8266_specs() -> list[DeviceSpec]:
    return [spec for spec in iter_device_specs() if spec.chip_family.startswith("ESP8266")]


class UiSchemaVersionTest(unittest.TestCase):
    """Text-level sweep, deliberately not structural.

    The structural checks below iterate fleet.yaml's `devices:` map, so they see
    only the ESPHome nodes; the Arduino sketches are a different language in a
    different part of the tree and were silently exempt. Grepping the sources
    catches every declaration regardless of file format, which is the only way
    this stays true for whatever device type comes next.
    """

    def test_no_device_source_declares_a_superseded_ui_schema(self) -> None:
        publishers = 0
        for glob in UI_SOURCE_GLOBS:
            for path in sorted(ROOT.glob(glob)):
                text = path.read_text(encoding="utf-8")
                if UI_CONFIG_SUFFIX not in text:
                    continue
                publishers += 1
                name = str(path.relative_to(ROOT))
                with self.subTest(source=name):
                    declared = set(SCHEMA_DECLARATION.findall(text))
                    # Not merely "no v1 found": a source that publishes the topic
                    # but whose schema this cannot read is the silent skip all
                    # over again, so an unreadable declaration fails too.
                    self.assertTrue(
                        declared,
                        f"{name} publishes {UI_CONFIG_SUFFIX} but declares no schema this "
                        f"check can read, so it is silently exempt from the version guard",
                    )
                    self.assertEqual(declared, {UI_SCHEMA}, f"{name} declares {sorted(declared)}")

        self.assertTrue(
            publishers, f"no source under {UI_SOURCE_GLOBS} publishes {UI_CONFIG_SUFFIX}"
        )


class UiConfigPayloadTest(unittest.TestCase):
    def test_v2_payloads_use_only_known_short_keys(self) -> None:
        for spec in iter_device_specs():
            found = ui_config_publish(spec)
            if found is None:
                continue
            document = json.loads(found[1])
            with self.subTest(device=spec.name):
                for group in document["groups"]:
                    self.assertLessEqual(
                        set(group), V2_GROUP_KEYS, f"unknown group key in {spec.name}"
                    )
                for entity in document["entities"]:
                    self.assertLessEqual(
                        set(entity), V2_ENTITY_KEYS, f"unknown entity key in {spec.name}"
                    )

    def test_every_esp8266_device_publishes_a_ui_config(self) -> None:
        """Also the size guard's coverage check: nothing below can measure an empty set."""
        specs = esp8266_specs()
        self.assertTrue(specs, "fleet.yaml declares no ESP8266 devices to guard")
        for spec in specs:
            with self.subTest(device=spec.name):
                self.assertIsNotNone(
                    ui_config_publish(spec),
                    f"{spec.name} publishes no {UI_CONFIG_SUFFIX}, so grow-app can "
                    f"never render it and the size guard silently skips it",
                )

    def test_ui_config_payloads_are_valid_json(self) -> None:
        measured = 0
        for spec in iter_device_specs():
            found = ui_config_publish(spec)
            if found is None:
                continue
            with self.subTest(device=spec.name):
                document = json.loads(found[1])
                self.assertEqual(document["schema"], UI_SCHEMA)
                self.assertEqual(document["nodeId"], spec.node_id)
                measured += 1
        self.assertGreater(measured, 0, "no _ui/config payload was located anywhere in the fleet")

    def test_esp8266_ui_config_packets_fit_the_tcp_send_buffer(self) -> None:
        specs = esp8266_specs()
        self.assertTrue(specs, "fleet.yaml declares no ESP8266 devices to guard")
        for spec in specs:
            found = ui_config_publish(spec)
            self.assertIsNotNone(found, f"{spec.name} lost its {UI_CONFIG_SUFFIX} publish")
            assert found is not None
            topic, payload = found
            with self.subTest(device=spec.name):
                size = packet_size(topic, payload)
                self.assertLess(
                    size,
                    ESP8266_TCP_SND_BUF,
                    f"{spec.name} _ui/config packet is {size} B, over the "
                    f"{ESP8266_TCP_SND_BUF} B ESP8266 send buffer; AsyncMqttClient "
                    f"will drop it silently and the device will not reach the dashboard",
                )


if __name__ == "__main__":
    unittest.main()

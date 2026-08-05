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
"""

from __future__ import annotations

import json
import re
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import iter_device_specs  # noqa: E402

# 2 x TCP_MSS on ESP8266. The packet is the payload plus a fixed header, the
# remaining-length varint, the topic and its length prefix, and (at qos 1) a
# packet id.
ESP8266_TCP_SND_BUF = 2920

TOPIC_PREFIX = "grow/daniel-home"


def extract_ui_payload(config: Path) -> str | None:
    """The device's literal `_ui/config` payload block, substitutions applied."""
    source = config.read_text(encoding="utf-8")
    marker = source.find("_ui/config")
    if marker < 0:
        return None

    start = source.index("payload: |-", marker) + len("payload: |-\n")
    lines: list[str] = []
    indent: int | None = None
    for line in source[start:].split("\n"):
        if not line.strip():
            break
        current = len(line) - len(line.lstrip())
        if indent is None:
            indent = current
        elif current < indent:
            break
        lines.append(line[indent:])

    payload = "\n".join(lines)
    substitutions = dict(re.findall(r"^\s{2}(\w+):\s+\"?([^\"\n]+)\"?$", source, re.M))
    for key, value in substitutions.items():
        payload = payload.replace(f"${{{key}}}", value.strip())
    return payload


def packet_size(payload: str, node_id: str) -> int:
    topic = f"{TOPIC_PREFIX}/{node_id}/_ui/config"
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


class UiConfigPayloadTest(unittest.TestCase):
    def test_every_ui_config_payload_is_valid_json(self) -> None:
        for spec in iter_device_specs():
            payload = extract_ui_payload(spec.config)
            if payload is None:
                continue
            with self.subTest(device=spec.name):
                document = json.loads(payload)
                self.assertEqual(document["schema"], "grow-ui.v1")
                self.assertEqual(document["nodeId"], spec.node_id)

    def test_esp8266_ui_config_packets_fit_the_tcp_send_buffer(self) -> None:
        for spec in iter_device_specs():
            if not spec.chip_family.startswith("ESP8266"):
                continue
            payload = extract_ui_payload(spec.config)
            if payload is None:
                continue
            with self.subTest(device=spec.name):
                size = packet_size(payload, spec.node_id)
                self.assertLess(
                    size,
                    ESP8266_TCP_SND_BUF,
                    f"{spec.name} _ui/config packet is {size} B, over the "
                    f"{ESP8266_TCP_SND_BUF} B ESP8266 send buffer; AsyncMqttClient "
                    f"will drop it silently and the device will not reach the dashboard",
                )


if __name__ == "__main__":
    unittest.main()

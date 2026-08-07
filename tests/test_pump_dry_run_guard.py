# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Guards the irrigation pump's dry-run cutout.

An empty reservoir leaves the SeaFlo pump unable to build pressure, so its
switch never opens and it runs until someone pulls the plug. The base package's
overcurrent trip cannot catch that -- ``current_limit`` is the plug's 16 A
rating and a dry pump draws LESS than a loaded one -- so the cutout is a
duration guard instead, and these tests pin the two properties that make it
safe to leave unattended.

Neither is visible at a glance in the YAML: a shortened timeout looks like a
tightened safety margin, and a trip that logs without opening the relay looks
like a working guard until the day it matters.
"""

from __future__ import annotations

import re
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import _load_device_config, device_spec  # noqa: E402

# grow-app clamps any single irrigation run to zones.max_run_seconds, 900 s
# today. The guard must sit clear of that ceiling: a legitimate long soak that
# trips the cutout latches the relay off and needs a human at the tent to rearm,
# which is worse than the fault the guard exists to catch. Lower this only
# alongside the grow-app clamp -- the two numbers are coupled, and the coupling
# is documented in devices/irrigation-pump.yaml.
GROW_APP_MAX_RUN_SECONDS = 900

DURATION = re.compile(r"^(\d+(?:\.\d+)?)\s*(ms|s|min|h)$")
UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "min": 60.0, "h": 3600.0}


def parse_duration(value: str) -> float:
    """Seconds from an ESPHome duration literal ("20min", "60s")."""
    match = DURATION.fullmatch(str(value).strip())
    if not match:
        raise AssertionError(f"unparseable ESPHome duration: {value!r}")
    return float(match.group(1)) * UNIT_SECONDS[match.group(2)]


class PumpDryRunGuardTest(unittest.TestCase):
    def setUp(self) -> None:
        self.config = _load_device_config(device_spec("irrigation-pump").config)
        self.subs = self.config.get("substitutions", {})

    def test_dry_run_timeout_clears_the_grow_app_run_clamp(self) -> None:
        timeout = parse_duration(self.subs["dry_run_timeout"])
        self.assertGreater(
            timeout,
            GROW_APP_MAX_RUN_SECONDS,
            f"dry_run_timeout is {timeout:.0f} s, at or under grow-app's "
            f"{GROW_APP_MAX_RUN_SECONDS} s single-run clamp — a legitimate long "
            "run would trip the cutout and latch the pump off until someone "
            "rearms it physically",
        )

    def test_cycle_bridge_is_shorter_than_the_timeout(self) -> None:
        """The bridge collapses pressure-switch cycling into one session; if it
        outlasted the timeout the guard could never arm."""
        bridge = parse_duration(self.subs["pump_cycle_bridge"])
        timeout = parse_duration(self.subs["dry_run_timeout"])
        self.assertLess(bridge, timeout, "pump_cycle_bridge must be well inside dry_run_timeout")

    def test_dry_run_trip_actually_opens_the_relay(self) -> None:
        """Logging a fault is not a cutout. The script must de-energize."""
        scripts = {entry["id"]: entry for entry in self.config["script"]}
        self.assertIn("dry_run_trip", scripts, "irrigation-pump declares no dry_run_trip script")
        actions = scripts["dry_run_trip"]["then"]
        turn_offs = [
            action["switch.turn_off"]
            for action in actions
            if isinstance(action, dict) and "switch.turn_off" in action
        ]
        self.assertIn(
            "pump_relay",
            turn_offs,
            "dry_run_trip must switch.turn_off pump_relay — a trip that only logs "
            "leaves the pump running dry",
        )


if __name__ == "__main__":
    unittest.main()

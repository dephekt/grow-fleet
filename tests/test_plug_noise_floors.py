# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Guards the plug metering's zero-suppression floors.

The baseline floors current and power to zero below a threshold to hide relay
leakage and standby noise. A load drawing less than that reads as exactly zero,
which is indistinguishable from an empty socket -- the exhaust fan ran for days
reporting 0 W while physically moving air, and the plug could not answer whether
it was on.

Neither property below is visible in a diff: raising a default silently blinds
every plug in the fleet, and deleting a device's override silently blinds that
one.
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import _load_device_config, device_spec  # noqa: E402

BASE_PACKAGE = ROOT / "devices" / "packages" / "athom-plug-base.yaml"

# The fleet-wide defaults. Pinned rather than read-and-compared so that raising
# them is a deliberate edit to this file, not a side effect somewhere else.
DEFAULT_CURRENT_FLOOR = 0.060
DEFAULT_POWER_FLOOR = 3.0

# Devices whose real draw sits under the defaults and which therefore must lower
# them, with the reason each one is on the list.
MUST_LOWER = {
    "exhaust-fan": "runs at a few watts; the defaults reported it as 0 W while moving air",
}


def substitutions(path: Path) -> dict:
    return _load_device_config(path).get("substitutions", {})


class PlugNoiseFloorTest(unittest.TestCase):
    def test_baseline_defaults_are_unchanged(self) -> None:
        subs = substitutions(BASE_PACKAGE)
        self.assertEqual(float(subs["current_noise_floor"]), DEFAULT_CURRENT_FLOOR)
        self.assertEqual(float(subs["power_noise_floor"]), DEFAULT_POWER_FLOOR)

    def test_small_load_devices_lower_both_floors(self) -> None:
        """An override that is not strictly lower is the same as no override."""
        for name, why in MUST_LOWER.items():
            with self.subTest(device=name):
                subs = substitutions(device_spec(name).config)
                self.assertIn("power_noise_floor", subs, f"{name} must lower its power floor — {why}")
                self.assertIn("current_noise_floor", subs, f"{name} must lower its current floor — {why}")
                self.assertLess(
                    float(subs["power_noise_floor"]),
                    DEFAULT_POWER_FLOOR,
                    f"{name}'s power floor must sit under the {DEFAULT_POWER_FLOOR} W default — {why}",
                )
                self.assertLess(
                    float(subs["current_noise_floor"]),
                    DEFAULT_CURRENT_FLOOR,
                    f"{name}'s current floor must sit under the {DEFAULT_CURRENT_FLOOR} A default — {why}",
                )

    def test_no_device_raises_a_floor_above_the_default(self) -> None:
        """Raising one hides a larger load, which is the bug in the other direction."""
        for spec in (device_spec(n) for n in MUST_LOWER):
            subs = substitutions(spec.config)
            self.assertLessEqual(float(subs.get("power_noise_floor", 0)), DEFAULT_POWER_FLOOR)
            self.assertLessEqual(float(subs.get("current_noise_floor", 0)), DEFAULT_CURRENT_FLOOR)


if __name__ == "__main__":
    unittest.main()

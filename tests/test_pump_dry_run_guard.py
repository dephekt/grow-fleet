# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Guards the irrigation pump's dry-run cutout.

An empty reservoir leaves the SeaFlo pump unable to build pressure, so its
switch never opens and it runs until someone pulls the plug. The base package's
overcurrent trip cannot catch that -- ``current_limit`` is the plug's 16 A
rating and a dry pump draws LESS than a loaded one -- so the cutout is a
duration guard instead, and these tests pin the properties that make it safe to
leave unattended.

None of them is visible at a glance in the YAML: a shortened timeout looks like
a tightened safety margin, a trip that logs without opening the relay looks like
a working guard, and a rearm that does not restart the session clock leaves a
guard that trips exactly once and never again.
"""

from __future__ import annotations

import re
import sys
import unittest
from collections.abc import Iterator
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

from fleetlib import _load_device_config, device_spec  # noqa: E402

# grow-app's per-zone run clamp, which the guard must sit clear of and which
# grow-app itself validates only as a positive integer.
GROW_APP_MAX_RUN_SECONDS = 600

# The base package zeroes any power sample under this, so a smaller draw
# threshold could never be reached.
PLUG_STANDBY_FLOOR_W = 3.0

DURATION = re.compile(r"(\d+(?:\.\d+)?)\s*(ms|s|min|h)")
UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "min": 60.0, "h": 3600.0}


def parse_duration(value: str) -> float:
    """Seconds from an ESPHome duration literal ("20min", "60s")."""
    match = DURATION.fullmatch(str(value).strip())
    if not match:
        raise AssertionError(f"unparseable ESPHome duration: {value!r}")
    return float(match.group(1)) * UNIT_SECONDS[match.group(2)]


def iter_actions(actions: Any) -> Iterator[dict[str, Any]]:
    """Every action in a ``then:`` list, descending into nested control flow."""
    if not isinstance(actions, list):
        return
    for action in actions:
        if not isinstance(action, dict):
            continue
        yield action
        for value in action.values():
            if isinstance(value, dict):
                yield from iter_actions(value.get("then"))
            elif isinstance(value, list):
                yield from iter_actions(value)


def action_values(actions: Any, key: str) -> list[Any]:
    """Values of every ``key`` action in the tree (e.g. every ``script.execute``)."""
    return [action[key] for action in iter_actions(actions) if key in action]


class PumpDryRunGuardTest(unittest.TestCase):
    def setUp(self) -> None:
        self.config = _load_device_config(device_spec("irrigation-pump").config)
        self.subs = self.config.get("substitutions", {})

    def substitute(self, value: str) -> str:
        for key, replacement in self.subs.items():
            value = value.replace(f"${{{key}}}", str(replacement))
        return value

    def script(self, script_id: str) -> dict[str, Any]:
        scripts = {
            entry["id"]: entry
            for entry in self.config.get("script", [])
            if isinstance(entry, dict) and "id" in entry
        }
        self.assertIn(script_id, scripts, f"irrigation-pump declares no {script_id} script")
        return scripts[script_id]

    @property
    def relay(self) -> dict[str, Any]:
        for entry in self.config.get("switch", []):
            if isinstance(entry, dict) and entry.get("id") == "pump_relay":
                return entry
        raise AssertionError("irrigation-pump declares no pump_relay switch")

    @property
    def pump_drawing(self) -> dict[str, Any]:
        for entry in self.config.get("binary_sensor", []):
            if isinstance(entry, dict) and entry.get("id") == "pump_drawing":
                return entry
        raise AssertionError("irrigation-pump declares no pump_drawing binary sensor")

    def test_draw_session_starts_and_ends_the_watchdog(self) -> None:
        """The session edges are the only thing that arms and disarms the clock."""
        sensor = self.pump_drawing
        self.assertIn(
            "dry_run_watchdog",
            action_values(sensor.get("on_press"), "script.execute"),
            "pump_drawing.on_press must start dry_run_watchdog or the guard never arms",
        )
        self.assertIn(
            "dry_run_watchdog",
            action_values(sensor.get("on_release"), "script.stop"),
            "pump_drawing.on_release must stop dry_run_watchdog — a clock left running "
            "after the pump stops trips on an idle pump and latches the relay off",
        )

    def test_draw_session_triggers_on_the_initial_state(self) -> None:
        """A binary sensor fires no on_press for its first state unless asked to."""
        sensor = self.pump_drawing
        self.assertTrue(
            sensor.get("trigger_on_initial_state") or sensor.get("publish_initial_state"),
            "pump_drawing must set trigger_on_initial_state — ESPHome gates the first "
            "state's callbacks, so a device that boots into a running pump (which "
            "RESTORE_DEFAULT_ON guarantees after a power blip) gets no on_press and "
            "leaves that entire session unguarded",
        )

    def test_draw_session_bridges_the_pressure_switch_cycling(self) -> None:
        """Without the bridge every pressure-switch gap restarts the session clock."""
        bridges = [
            parse_duration(self.substitute(str(entry["delayed_off"])))
            for entry in self.pump_drawing.get("filters") or []
            if isinstance(entry, dict) and "delayed_off" in entry
        ]
        self.assertEqual(
            bridges,
            [parse_duration(self.subs["pump_cycle_bridge"])],
            "pump_drawing must carry exactly one delayed_off of ${pump_cycle_bridge}",
        )

    def test_draw_threshold_clears_the_base_package_standby_floor(self) -> None:
        """A threshold under the floor is unreachable, so the guard would never arm."""
        threshold = float(self.subs["pump_draw_min_w"])
        self.assertGreater(
            threshold,
            PLUG_STANDBY_FLOOR_W,
            f"pump_draw_min_w is {threshold} W, at or under the base package's "
            f"{PLUG_STANDBY_FLOOR_W} W standby suppression",
        )
        self.assertIn(
            "${pump_draw_min_w}",
            str(self.pump_drawing.get("lambda", "")),
            "pump_drawing must read the substituted threshold, not a hardcoded literal",
        )

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

    def test_watchdog_waits_the_dry_run_timeout(self) -> None:
        """The session clock is the watchdog's delay, not the substitution alone."""
        watchdog = self.script("dry_run_watchdog")
        delays = [
            parse_duration(self.substitute(str(v)))
            for v in action_values(watchdog["then"], "delay")
        ]
        self.assertEqual(
            delays,
            [parse_duration(self.subs["dry_run_timeout"])],
            "dry_run_watchdog must wait exactly ${dry_run_timeout} before tripping",
        )
        self.assertIn(
            "dry_run_trip",
            action_values(watchdog["then"], "script.execute"),
            "dry_run_watchdog must end in dry_run_trip",
        )

    def test_watchdog_restarts_rather_than_stacking(self) -> None:
        """A second session start must reset the clock, not queue a second trip."""
        self.assertEqual(self.script("dry_run_watchdog").get("mode"), "restart")

    def test_dry_run_trip_actually_opens_the_relay(self) -> None:
        """Logging a fault is not a cutout. The script must de-energize."""
        turn_offs = action_values(self.script("dry_run_trip")["then"], "switch.turn_off")
        self.assertIn(
            "pump_relay",
            turn_offs,
            "dry_run_trip must switch.turn_off pump_relay — a trip that only logs "
            "leaves the pump running dry",
        )

    def test_dry_run_trip_raises_the_fault_status(self) -> None:
        """The latch is what grow-app and the status LED both read."""
        lambdas = " ".join(
            str(v) for v in action_values(self.script("dry_run_trip")["then"], "lambda")
        )
        self.assertIn(
            "status_set_error",
            lambdas,
            "dry_run_trip must raise pump_relay's error status or the trip is silent",
        )

    def test_rearm_restarts_the_session_clock(self) -> None:
        """Rearming inside ${pump_cycle_bridge} leaves pump_drawing latched on, so
        no on_press follows and only on_turn_on can restart the watchdog.

        Without this the guard fires exactly once: the delayed_off bridge swallows
        the relay-off, ESPHome's per-filter dedup drops the redundant on-edge that
        follows, and the pump then runs dry indefinitely.
        """
        on_turn_on = self.relay.get("on_turn_on")
        self.assertIn(
            "dry_run_watchdog",
            action_values(on_turn_on, "script.execute"),
            "pump_relay.on_turn_on must restart dry_run_watchdog — a rearm within "
            "the cycle bridge produces no pump_drawing edge, so the guard would "
            "never arm again",
        )

    def test_rearm_clears_the_latched_fault(self) -> None:
        """Energizing the supply is the only rearm; nothing else clears the error."""
        lambdas = " ".join(str(v) for v in action_values(self.relay.get("on_turn_on"), "lambda"))
        self.assertIn(
            "status_clear_error",
            lambdas,
            "pump_relay.on_turn_on must clear the error status or the fault latches forever",
        )


if __name__ == "__main__":
    unittest.main()

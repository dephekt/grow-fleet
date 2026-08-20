# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider

"""Guards the humidifier plug's command watchdog.

The humidifier is the fleet's only purely loop-owned actuator: with grow-app
gone there is nothing local that knows whether the tent wants humidity, so the
relay opens after a few minutes of MQTT silence and the tent ends up as dry as
the room rather than saturated.

Every property below is invisible at a glance in the YAML, and each failure mode
looks like a working guard:

  - a subscription pointed at a topic the relay does not actually publish on
    never hears grow-app's keepalive, so the watchdog fires mid-run on EVERY
    run -- and because the loop re-engages only at the 1.2 kPa hard ceiling, an
    early cut leaves the humidifier off until the tent drifts back over the
    ceiling it was hired to defend;
  - a countdown that stacks instead of restarting trips on the schedule of the
    FIRST command rather than the last;
  - an expiry that does not re-check the relay raises a fault on a relay
    somebody legitimately switched off;
  - a trip that logs without opening the relay reads exactly like one that works.
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

BASE_PACKAGE = ROOT / "devices" / "packages" / "athom-plug-base.yaml"

# grow-app's climate tick (GROW_CLIMATE_TICK_SECONDS), which is also the cadence
# of the keepalive that holds this watchdog off -- see humidifierKeepalive in
# grow-app's src/lib/server/climate/loop.ts.
GROW_APP_TICK_SECONDS = 10

# Missed keepalives the window must absorb before it trips. A tick is a timer on
# a busy event loop and runs late; a window only a few ticks wide would fail dry
# on a slow GC rather than on an actual outage.
MIN_MISSED_KEEPALIVES = 6

# The base package zeroes any power sample under this, so a smaller draw
# threshold could never be reached.
PLUG_STANDBY_FLOOR_W = 3.0

DURATION = re.compile(r"(\d+(?:\.\d+)?)\s*(ms|s|min|h)")
UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "min": 60.0, "h": 3600.0}


def parse_duration(value: str) -> float:
    """Seconds from an ESPHome duration literal ("60s", "5min")."""
    match = DURATION.fullmatch(str(value).strip())
    if not match:
        raise AssertionError(f"unparseable ESPHome duration: {value!r}")
    return float(match.group(1)) * UNIT_SECONDS[match.group(2)]


def object_id(name: str) -> str:
    """The objectId ESPHome derives from an entity name, which is what lands in
    the MQTT topic and in grow-app's entity refs."""
    return re.sub(r"[^a-z0-9]+", "_", str(name).lower()).strip("_")


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


class HumidifierWatchdogTest(unittest.TestCase):
    def setUp(self) -> None:
        self.spec = device_spec("humidifier")
        self.config = _load_device_config(self.spec.config)
        self.base = _load_device_config(BASE_PACKAGE)
        self.source = self.spec.config.read_text(encoding="utf-8")
        # The device's own substitutions win over the package defaults it overrides.
        self.subs = {**self.base.get("substitutions", {}), **self.config.get("substitutions", {})}

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
        self.assertIn(script_id, scripts, f"humidifier declares no {script_id} script")
        return scripts[script_id]

    @property
    def relay(self) -> dict[str, Any]:
        for entry in self.config.get("switch", []):
            if isinstance(entry, dict) and entry.get("id") == "humidifier_relay":
                return entry
        raise AssertionError("humidifier declares no humidifier_relay switch")

    @property
    def timeout(self) -> dict[str, Any]:
        for entry in self.config.get("number", []):
            if isinstance(entry, dict) and entry.get("id") == "command_watchdog":
                return entry
        raise AssertionError("humidifier declares no command_watchdog number")

    def binary_sensor(self, sensor_id: str) -> dict[str, Any]:
        for entry in self.config.get("binary_sensor", []):
            if isinstance(entry, dict) and entry.get("id") == sensor_id:
                return entry
        raise AssertionError(f"humidifier declares no {sensor_id} binary sensor")

    # ── The keepalive subscription ────────────────────────────────────────────

    def test_watchdog_listens_on_the_relays_real_command_topic(self) -> None:
        """ESPHome builds a switch's command topic as
        ``<topic_prefix>/switch/<objectId>/command``. The watchdog subscribes to
        that topic literally, so a renamed relay silently moves the topic and
        leaves the subscription listening to nothing -- a watchdog that then
        fires on every run instead of never."""
        prefix = self.substitute(str(self.base["mqtt"]["topic_prefix"]))
        expected = f"{prefix}/switch/{object_id(self.relay['name'])}/command"
        topics = [
            self.substitute(str(entry["topic"]))
            for entry in self.config["mqtt"].get("on_message", [])
            if isinstance(entry, dict) and "topic" in entry
        ]
        self.assertEqual(
            topics,
            [expected],
            "the watchdog must subscribe to exactly the relay's own command topic",
        )

    def test_relay_object_id_substitution_matches_the_relays_name(self) -> None:
        """``${relay_object_id}`` also names the switch in the _ui/config payload,
        so a drift here hides the relay from grow-app's dashboard as well."""
        self.assertEqual(self.subs["relay_object_id"], object_id(self.relay["name"]))

    def test_every_command_kicks_the_watchdog(self) -> None:
        """A repeat of the state the relay is already in IS the keepalive, and
        ESPHome fires on_turn_on only on a change -- so the subscription, not the
        relay trigger, is what has to restart the countdown."""
        for entry in self.config["mqtt"]["on_message"]:
            self.assertIn(
                "watchdog_kick",
                action_values(entry.get("then"), "script.execute"),
                "the command subscription must restart the countdown",
            )

    def test_closing_the_relay_arms_the_countdown(self) -> None:
        """The physical button publishes no command, so without this a
        button-started run would be unguarded until grow-app happened to speak."""
        self.assertIn(
            "watchdog_kick",
            action_values(self.relay.get("on_turn_on"), "script.execute"),
            "humidifier_relay.on_turn_on must start watchdog_kick",
        )

    # ── The countdown ─────────────────────────────────────────────────────────

    def test_watchdog_restarts_rather_than_stacking(self) -> None:
        """Every keepalive re-executes this script; without restart mode they
        queue and the relay opens on the first command's schedule."""
        self.assertEqual(self.script("watchdog_kick").get("mode"), "restart")

    def test_countdown_reads_the_timeout_entity_in_minutes(self) -> None:
        """The delay is a lambda, so it is unreachable structurally -- and a 1000
        where 60000 belongs turns a five-minute watchdog into a five-second one
        that opens the relay in the middle of every run."""
        delays = action_values(self.script("watchdog_kick")["then"], "delay")
        self.assertEqual(len(delays), 1, "watchdog_kick must have exactly one delay")
        self.assertRegex(
            self.source,
            r"delay:\s*!lambda.*id\(command_watchdog\)\.state\s*\*\s*60000",
            "the countdown must convert the timeout entity's minutes to ms",
        )

    def test_expiry_rechecks_the_relay_before_tripping(self) -> None:
        """The countdown is deliberately never stopped when the relay opens
        (stopping a script from a trigger that script fired is the reentrancy
        this avoids), so the expiry has to notice a relay that is already off or
        it raises a fault on a legitimate manual OFF."""
        guards = [
            action["if"]
            for action in iter_actions(self.script("watchdog_kick")["then"])
            if "if" in action
            and action["if"].get("condition", {}).get("switch.is_on") == "humidifier_relay"
        ]
        self.assertEqual(len(guards), 1, "the expiry must be guarded on the relay still being on")
        self.assertIn(
            "humidifier_relay",
            action_values(guards[0].get("then"), "switch.turn_off"),
            "the guarded branch is what opens the relay",
        )

    def test_trip_opens_the_relay_and_raises_the_annunciator(self) -> None:
        """A trip that logs without opening the relay reads exactly like one that
        works, and one that opens it without the error status leaves the
        dashboard unable to tell a watchdog cut from a normal OFF."""
        then = self.script("watchdog_kick")["then"]
        self.assertIn("humidifier_relay", action_values(then, "switch.turn_off"))
        self.assertIn(
            "status_set_error",
            " ".join(str(v) for v in action_values(then, "lambda")),
            "the trip must raise humidifier_relay's error status",
        )
        self.assertEqual(
            object_id(self.binary_sensor("watchdog_trip")["name"]),
            "watchdog_trip",
            "the _ui/config payload names this sensor watchdog_trip",
        )

    # ── The timeout entity ────────────────────────────────────────────────────

    def test_watchdog_defaults_armed_and_survives_missed_keepalives(self) -> None:
        default = float(self.substitute(str(self.timeout["initial_value"]))) * 60.0
        self.assertGreater(default, 0.0, "the watchdog must default to armed, not disabled")
        self.assertGreaterEqual(
            default,
            MIN_MISSED_KEEPALIVES * GROW_APP_TICK_SECONDS,
            f"a {default:.0f} s window absorbs fewer than {MIN_MISSED_KEEPALIVES} missed "
            f"{GROW_APP_TICK_SECONDS} s keepalives, so a late tick fails the tent dry "
            "while grow-app is perfectly healthy",
        )

    def test_watchdog_can_be_disabled_for_bench_work(self) -> None:
        """0 is the documented escape hatch; the countdown's own guard is what
        honours it, so the entity has to be able to reach it."""
        self.assertEqual(float(self.timeout["min_value"]), 0.0)
        self.assertTrue(self.timeout.get("restore_value"), "the timeout is desired state")

    # ── Fail-dry and the load ─────────────────────────────────────────────────

    def test_relay_never_resumes_misting_after_a_power_blip(self) -> None:
        """Across a reboot the loop's intent is unknowable, and it re-commands
        within a tick if it still wants humidity."""
        self.assertEqual(self.relay.get("restore_mode"), "ALWAYS_OFF")

    def test_overcurrent_opens_the_relay(self) -> None:
        self.assertIn(
            "humidifier_relay",
            action_values(self.script("overcurrent_trip")["then"], "switch.turn_off"),
        )

    def test_misting_threshold_clears_the_base_package_standby_floor(self) -> None:
        """A threshold under the floor is unreachable, so a dry tank would read
        as normal misting forever."""
        threshold = float(self.subs["misting_min_w"])
        self.assertGreater(
            threshold,
            PLUG_STANDBY_FLOOR_W,
            f"misting_min_w is {threshold} W, at or under the base package's "
            f"{PLUG_STANDBY_FLOOR_W} W standby suppression",
        )
        self.assertIn(
            "${misting_min_w}",
            str(self.binary_sensor("not_misting").get("lambda", "")),
            "not_misting must read the substituted threshold, not a hardcoded literal",
        )

    def test_not_misting_waits_out_the_spin_up(self) -> None:
        """The head takes a moment to raise a mist and the meter averages over
        ${sensor_update_interval}; without the grace every start reads as a fault."""
        graces = [
            parse_duration(self.substitute(str(entry["delayed_on"])))
            for entry in self.binary_sensor("not_misting").get("filters") or []
            if isinstance(entry, dict) and "delayed_on" in entry
        ]
        self.assertEqual(
            graces,
            [parse_duration(self.subs["misting_grace"])],
            "not_misting must carry exactly one delayed_on of ${misting_grace}",
        )
        self.assertGreater(
            graces[0],
            parse_duration(self.subs["sensor_update_interval"]),
            "the grace must outlast one metering interval or the first averaged "
            "sample of a healthy start still reads as a fault",
        )


if __name__ == "__main__":
    unittest.main()

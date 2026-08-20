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

# grow-app's climate tick (GROW_CLIMATE_TICK_SECONDS), which is also the cadence
# of the keepalive that holds this watchdog off -- see humidifierKeepalive in
# grow-app's src/lib/server/climate/loop.ts.
GROW_APP_TICK_SECONDS = 10

# Missed keepalives the window must absorb before it trips. A tick is a timer on
# a busy event loop and runs late; a window only a few ticks wide would fail dry
# on a slow GC rather than on an actual outage.
MIN_MISSED_KEEPALIVES = 6

DURATION = re.compile(r"(\d+(?:\.\d+)?)\s*(ms|s|min|h)")
UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "min": 60.0, "h": 3600.0}


def parse_duration(value: str) -> float:
    """Seconds from an ESPHome duration literal ("60s", "5min")."""
    match = DURATION.fullmatch(str(value).strip())
    if not match:
        raise AssertionError(f"unparseable ESPHome duration: {value!r}")
    return float(match.group(1)) * UNIT_SECONDS[match.group(2)]


POWER_CLAMP = re.compile(r"x\s*<\s*([\d.]+)")


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
        self.source = self.spec.config.read_text(encoding="utf-8")
        # Self-contained: this plug is an ESP32-C3 and consumes no package, so every
        # substitution the assertions below read is declared in the device file itself.
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
        self.assertIn(script_id, scripts, f"humidifier declares no {script_id} script")
        return scripts[script_id]

    @property
    def relay(self) -> dict[str, Any]:
        for entry in self.config.get("switch", []):
            if isinstance(entry, dict) and entry.get("id") == "humidifier_relay":
                return entry
        raise AssertionError("humidifier declares no humidifier_relay switch")

    @property
    def standby_floor_w(self) -> float:
        """The clamp on plug_power, read from this device rather than restated.

        It used to be a constant here, copied from packages/athom-plug-base.yaml
        -- which this device does not consume; its clamp is inline. A copy passes
        forever after the original moves: raise the inline clamp to 10 W while
        chasing a noisy meter and a threshold anywhere in 4-10 W becomes settable
        again, where plug_power is floored to exactly 0.0 and a running T7 never
        registers as misting. That is the inverse of the defect the guard exists
        for, and a restated constant cannot see it.
        """
        for entry in self.config.get("sensor", []):
            if not isinstance(entry, dict) or entry.get("platform") != "cse7766":
                continue
            for filt in entry.get("power", {}).get("filters", []) or []:
                if not isinstance(filt, dict):
                    continue
                match = POWER_CLAMP.search(str(filt.get("lambda", "")))
                if match:
                    return float(match.group(1))
        raise AssertionError("humidifier declares no standby clamp on plug_power")

    def number(self, number_id: str) -> dict[str, Any]:
        for entry in self.config.get("number", []):
            if isinstance(entry, dict) and entry.get("id") == number_id:
                return entry
        raise AssertionError(f"humidifier declares no {number_id} number")

    @property
    def timeout(self) -> dict[str, Any]:
        return self.number("command_watchdog")

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
        prefix = self.substitute(str(self.config["mqtt"]["topic_prefix"]))
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

    def test_each_trip_annunciates_as_itself_not_as_the_other(self) -> None:
        """Both trips open the same relay and both raise its single error status
        to drive the LED, so a sensor reading that status reports whichever fault
        happened as the other one. An overcurrent shown as "Watchdog Trip" is a
        claim about the network when the fault is electrical, and the two want
        opposite responses -- the watchdog clears when grow-app returns, the
        overcurrent does not. irrigation-pump avoids the collision by leaving its
        overcurrent silent; this plug cannot, because its ERROR log reaches
        nobody (baud_rate 0, log_topic NONE, no API)."""
        flags = {
            "watchdog_trip": "watchdog_latched",
            "overcurrent_fault": "overcurrent_latched",
        }
        for sensor_id, flag in flags.items():
            lam = str(self.binary_sensor(sensor_id).get("lambda", ""))
            self.assertIn(f"id({flag})", lam)
            self.assertNotIn(
                "status_has_error",
                lam,
                f"{sensor_id} must read its own flag, not the shared error status",
            )

        # Each script raises exactly its own flag.
        for script_id, own in (
            ("watchdog_kick", "watchdog_latched"),
            ("overcurrent_trip", "overcurrent_latched"),
        ):
            body = " ".join(str(v) for v in action_values(self.script(script_id)["then"], "lambda"))
            other = next(f for f in flags.values() if f != own)
            self.assertIn(f"id({own}) = true", body)
            self.assertNotIn(f"id({other}) = true", body)

    def test_closing_the_relay_clears_both_annunciators(self) -> None:
        """Closing the relay is the only rearm, so a flag it does not clear is a
        fault that never goes away -- the globals are restore_value: no, so a
        reboot is the only other thing that clears them."""
        body = " ".join(str(v) for v in action_values(self.relay.get("on_turn_on"), "lambda"))
        for flag in ("watchdog_latched", "overcurrent_latched"):
            self.assertIn(f"id({flag}) = false", body)

    def test_latched_faults_allocate_no_stored_state(self) -> None:
        """A latched fault must not survive the power cycle that cleared the
        condition; the relay comes back at its restored state regardless."""
        for entry in self.config.get("globals", []):
            if entry.get("id") in ("watchdog_latched", "overcurrent_latched"):
                self.assertFalse(entry.get("restore_value"), f"{entry['id']} must not persist")

    def test_trip_opens_the_relay_and_raises_the_annunciator(self) -> None:
        """A trip that logs without opening the relay reads exactly like one that
        works, and one that opens it without setting the flag leaves the dashboard
        unable to tell a watchdog cut from a normal OFF."""
        then = self.script("watchdog_kick")["then"]
        self.assertIn("humidifier_relay", action_values(then, "switch.turn_off"))
        self.assertIn(
            "id(watchdog_latched) = true",
            " ".join(str(v) for v in action_values(then, "lambda")),
        )
        self.assertEqual(
            object_id(self.binary_sensor("watchdog_trip")["name"]),
            "watchdog_trip",
            "the _ui/config payload names this sensor watchdog_trip",
        )

    def test_only_a_fault_needing_a_human_drives_the_status_led(self) -> None:
        """The LED is one bit shared by everything, so what may raise it is a
        policy question. A watchdog cut is expected and self-correcting, and it
        clears only when the relay is re-energized — which after a trip may be a
        whole night away, since grow-app publishes nothing to an open relay. An
        LED left flashing that long trains the operator to ignore it, and
        overcurrent, which genuinely needs a human, has nothing else."""
        watchdog = " ".join(
            str(v) for v in action_values(self.script("watchdog_kick")["then"], "lambda")
        )
        overcurrent = " ".join(
            str(v) for v in action_values(self.script("overcurrent_trip")["then"], "lambda")
        )
        self.assertNotIn("status_set_error", watchdog)
        self.assertIn("status_set_error", overcurrent)

    def test_watchdog_trip_is_state_not_an_alert(self) -> None:
        """grow-app treats a device_class: problem binary_sensor as an alert
        entity (threshold-match.ts isAlertEntity). This one would be a false
        positive in both directions: it cannot fire in the case it exists for,
        because the condition IS grow-app being gone and a grow-app that is gone
        raises nothing; and in the case that actually happens — a redeploy that
        outlasts the timeout — it sticks true for hours, because after the trip
        the relay is open, the keepalive stops (it only re-asserts a relay that
        is ON) and decideClimate re-engages only at the 1.2 kPa ceiling. Same
        reasoning that keeps `misting` off the alert surface."""
        self.assertIsNone(self.binary_sensor("watchdog_trip").get("device_class"))
        self.assertEqual(self.binary_sensor("overcurrent_fault").get("device_class"), "problem")

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

    def test_preference_flush_is_deferred_past_the_value_it_is_flushing(self) -> None:
        """TemplateNumber::control() publishes -- firing on_value -- BEFORE it
        calls pref_.save(), and on ESP32 save() only queues into s_pending_save
        while sync() is what commits. An inline flush therefore commits
        everything EXCEPT the value that triggered it, and the new one waits out
        flash_write_interval (5 min here).

        That is not cosmetic: the header's own instruction for the observation
        phase is to set Cut Off After to 0, and losing mains inside that window
        boots the plug back at 5, whereupon the watchdog opens the relay and the
        humidifier goes dark -- precisely what the flush exists to prevent.

        Switch::publish_state saves BEFORE its callback, which is why the relay's
        flush needs no delay and why the asymmetry is easy to miss."""
        for number_id in ("command_watchdog", "misting_threshold"):
            actions = self.number(number_id)["on_value"]
            delays = [i for i, a in enumerate(actions) if "delay" in a]
            syncs = [
                i
                for i, a in enumerate(actions)
                if "global_preferences->sync" in str(a.get("lambda", ""))
            ]
            self.assertTrue(delays, f"{number_id}.on_value must defer its flush")
            self.assertTrue(syncs, f"{number_id}.on_value must flush at all")
            self.assertLess(
                delays[0],
                syncs[0],
                f"{number_id} flushes before control() saves — the change is lost",
            )

    def test_watchdog_can_be_disabled_for_bench_work(self) -> None:
        """0 is the documented escape hatch; the countdown's own guard is what
        honours it, so the entity has to be able to reach it."""
        self.assertEqual(float(self.timeout["min_value"]), 0.0)
        self.assertTrue(self.timeout.get("restore_value"), "the timeout is desired state")

    # ── Fail-dry and the load ─────────────────────────────────────────────────

    def test_targets_the_esp32_c3_the_plug_actually_is(self) -> None:
        """An Athom V3 is an ESP32-C3 and shares NO pin assignment with the
        ESP8285 V2 the rest of the fleet's plugs are — the V2's button pin is the
        V3's relay pin. Consuming the ESP8266 plug base here, or copying its pin
        numbers, produces an image that drives a relay from a pulled-up input."""
        self.assertNotIn(
            "athom-plug-base.yaml",
            str(self.config.get("packages", {})),
            "the ESP8266 plug baseline cannot be consumed by an ESP32-C3 device",
        )
        self.assertEqual(self.config["esp32"]["variant"], "ESP32C3")
        self.assertNotIn("esp8266", self.config)
        self.assertEqual(self.relay["pin"], "GPIO5")
        self.assertEqual(self.config["uart"]["rx_pin"], "GPIO20")
        self.assertIn(
            '"chipFamily": "ESP32-C3"',
            self.source,
            "the _firmware/config payload feeds grow-app's OTA manifest matching",
        )

    def test_relay_restores_its_pre_blip_state_and_flushes_both_edges(self) -> None:
        """RESTORE_DEFAULT_OFF is only safe because the watchdog bounds an
        unsupervised resume; the flush is what makes the restored state the real
        one rather than whatever the 1 h preference interval last happened to
        catch."""
        self.assertEqual(self.relay.get("restore_mode"), "RESTORE_DEFAULT_OFF")
        for edge in ("on_turn_on", "on_turn_off"):
            self.assertIn(
                "global_preferences->sync",
                " ".join(str(v) for v in action_values(self.relay.get(edge), "lambda")),
                f"humidifier_relay.{edge} must flush, or a blip restores a stale state",
            )

    def test_overcurrent_opens_the_relay(self) -> None:
        self.assertIn(
            "humidifier_relay",
            action_values(self.script("overcurrent_trip")["then"], "switch.turn_off"),
        )

    def test_misting_threshold_default_clears_the_standby_floor(self) -> None:
        """A default at or under the floor would read a resting T7 as misting."""
        threshold = float(self.substitute(str(self.number("misting_threshold")["initial_value"])))
        floor = self.standby_floor_w
        self.assertGreater(
            threshold,
            floor,
            f"the Misting Threshold default is {threshold} W, at or under the "
            f"{floor} W standby suppression on plug_power",
        )

    def test_misting_threshold_cannot_be_typed_into_the_clamped_range(self) -> None:
        """plug_power is clamped to exactly 0.0 below the standby floor, and the
        misting lambda is `>=`, so a threshold at or under the clamp makes
        `0.0 >= threshold` true while resting and the observation record silently
        becomes a copy of the relay state. The entity is a box the operator types
        into live, so the unreachable range is made unreachable rather than
        merely documented."""
        self.assertGreater(
            float(self.number("misting_threshold")["min_value"]),
            self.standby_floor_w,
            "min_value must sit above the standby clamp on plug_power",
        )

    def test_misting_reads_the_tunable_threshold_not_a_baked_constant(self) -> None:
        """The observation phase is what calibrates the threshold, so it has to
        move without a reflash -- and a NaN restore must not read as misting."""
        lam = str(self.binary_sensor("misting").get("lambda", ""))
        self.assertIn("id(misting_threshold).state", lam)
        self.assertIn("isnan", lam, "an unrestored threshold must not read as misting")
        self.assertTrue(self.number("misting_threshold").get("restore_value"))

    def test_misting_is_a_plain_state_not_a_fault(self) -> None:
        """Through the observation phase an idle T7 is the humidistat resting.
        A problem class would sit red for most of every day and feed grow-app's
        alert conditions with it; whether idle is a fault depends on who owns the
        relay, which only grow-app knows."""
        self.assertIsNone(self.binary_sensor("misting").get("device_class"))

    def test_misting_carries_no_edge_filters(self) -> None:
        """The sensor is a pure function of the published power series, so its
        resolution is the meter's throttle_average and nothing else.

        It used to carry a 20 s delayed_on, sized when it was a FAULT detector
        that had to not cry wolf during spin-up. As a state sensor that only
        deleted the front of every episode: the T7 ramps its output 1->6 over
        ~10 s and re-fires in bursts as short as 30 s, so the grace dropped the
        whole leading ramp and any short burst with it. The ramp IS the load.

        And no delayed_off, though irrigation-pump's pump_drawing carries one for
        what looks like the same shape. It is not the same: that bridge spans a
        pressure switch cycling WITHIN one shot, where the shot is the unit of
        interest. Here the zero gaps are the T7 genuinely stopping and re-deciding
        once the fans have spread the mist, and the re-fire cadence is itself a
        reading on how far its sensor disagrees with ours. Bridging erases it.
        """
        self.assertFalse(
            self.binary_sensor("misting").get("filters"),
            "misting must report the power series as measured, both edges",
        )

if __name__ == "__main__":
    unittest.main()

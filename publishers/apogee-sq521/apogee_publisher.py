#!/usr/bin/env python3
"""Publish Apogee SQ-521 (SDI-12) readings to the grow/daniel-home MQTT bus.

The SQ-521 is a full-spectrum quantum (PAR) sensor with an SDI-12 digital output,
wired through a Dr. Liu SDI-12->USB adapter (an FTDI serial port, /dev/ttyUSB0) on
quantum-sensor.home.arpa. This is NOT an ESPHome device, so it hand-rolls the same
Home-Assistant-style discovery + retained-scalar-state conventions the ESP32 fleet
uses (grow-fleet/devices/*.yaml) — grow-app then auto-registers it and
grow-history-recorder captures it, with zero server-side changes.

Entities published (all under device "quantum-sensor"):
  ppfd          Canopy PPFD, umol/s/m^2   (primary, historised)
  detector_mv   raw detector millivolts   (historised)
  tilt          angle from vertical, deg  (historised; SQ-521 serial >= 3033)
  sensor_serial sensor serial number      (static, diagnostic)
  cal_factor    calibration multiplier    (static, diagnostic)

SDI-12 command map (Apogee SQ-521 manual, address 0):
  0M!  / 0D0!  -> PPFD (umol m-2 s-1)
  0M1! / 0D0!  -> detector millivolts
  0M4! / 0D0!  -> tilt angle from vertical (deg)
  0V!  / 0D0!  -> calibration coefficients (multiplier, offset, ...)
  0I!          -> identification (vendor, model, firmware, serial)
"""
import json
import os
import re
import signal
import time

import serial
import paho.mqtt.client as mqtt

# ---- configuration (environment) ----
NODE_ID = os.environ.get("NODE_ID", "quantum-sensor")
MQTT_HOST = os.environ.get("MQTT_HOST", "192.168.8.3")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1883"))
MQTT_USERNAME = os.environ.get("MQTT_USERNAME", "edge-daniel-home")
MQTT_PASSWORD = os.environ.get("MQTT_PASSWORD", "")
SITE_PREFIX = os.environ.get("MQTT_TOPIC_PREFIX", "grow/daniel-home")
SDI12_PORT = os.environ.get("SDI12_PORT", "/dev/ttyUSB0")
SDI12_ADDRESS = os.environ.get("SDI12_ADDRESS", "0")
POLL_SECONDS = float(os.environ.get("POLL_SECONDS", "10"))
PPFD_UNIT = os.environ.get("PPFD_UNIT", "µmol/s/m²")

BASE = f"{SITE_PREFIX}/{NODE_ID}"  # the discovery `~` base
DISCOVERY_PREFIX = f"{SITE_PREFIX}/_discovery"
STATUS_TOPIC = f"{BASE}/status"

# Shared device block so every entity groups into one device card in grow-app.
DEVICE = {
    "identifiers": [NODE_ID],
    "name": "Quantum PAR Sensor",
    "manufacturer": "Apogee Instruments",
    "model": "SQ-521",
}


def log(*args):
    print("[apogee]", *args, flush=True)


def parse_first(resp):
    """First signed number in an SDI-12 data response like '0+156.9289' -> 156.93."""
    if not resp:
        return None
    body = resp[1:] if resp[:1].isalnum() else resp  # drop the leading address char
    found = re.findall(r"[+-]?\d+(?:\.\d+)?", body)
    return float(found[0]) if found else None


def parse_identity(ident):
    """SDI-12 I! layout: addr(1) version(2) vendor(8) model(6) fw(3) serial(<=13)."""
    if not ident or len(ident) < 20:
        return (None, None)
    fw = ident[17:20].strip()
    serial_no = ident[20:].strip()
    return (fw or None, serial_no or None)


def parse_atttn(header):
    """SDI-12 measurement response 'atttn' -> (ttt seconds until ready, n values)."""
    if not header:
        return (0, 0)  # no response — command unsupported/failed; skip (don't read a stale D0!)
    match = re.match(r"^.(\d{3})(\d+)$", header)
    if not match:
        return (1, 1)  # present but non-standard: assume 1 value ready in ~1 s
    return (int(match.group(1)), int(match.group(2)))


class Sdi12:
    """Minimal client for the Dr. Liu SDI-12->USB adapter (9600 8N1, raw ASCII commands)."""

    def __init__(self, port, address):
        self.port = port
        self.address = address
        self.ser = None
        self.open()

    def open(self):
        self.ser = serial.Serial(self.port, 9600, timeout=2)
        # Opening the port toggles DTR and resets the adapter's MCU; wait for its boot
        # plus the adapter's internal ~1 s startup delay before the first command.
        time.sleep(2.5)

    def _read_line(self):
        return self.ser.readline().decode("ascii", "replace").strip()

    def _cmd(self, cmd):
        self.ser.reset_input_buffer()
        self.ser.write(cmd.encode("ascii"))
        return self._read_line()

    def _transaction(self, cmd):
        """M/V-style command: <cmd> -> 'atttn', wait ttt for the service request, then aD0!."""
        self.ser.reset_input_buffer()
        self.ser.write(cmd.encode("ascii"))
        ttt, n = parse_atttn(self._read_line())  # 'atttn': seconds-until-ready, value count
        if n <= 0:
            return None  # sensor advertises no values for this command (e.g. tilt on old units)
        if ttt > 0:
            # The sensor sends its address (a service request) when data is ready; allow up to ttt
            # seconds for it. ttt=0 means the data is ready now with no service request to wait on.
            prev = self.ser.timeout
            self.ser.timeout = ttt + 1
            try:
                self._read_line()
            finally:
                self.ser.timeout = prev
        return parse_first(self._cmd(f"{self.address}D0!"))

    def identify(self):
        return self._cmd(f"{self.address}I!")

    def measure(self, sub=""):
        return self._transaction(f"{self.address}M{sub}!")

    def verify(self):
        return self._transaction(f"{self.address}V!")

    def close(self):
        try:
            if self.ser:
                self.ser.close()
        except Exception:
            pass


def discovery_config(object_id, name, **fields):
    cfg = {
        "~": BASE,
        "name": name,
        "object_id": object_id,
        "unique_id": f"{NODE_ID}_{object_id}".replace("-", "_"),
        "state_topic": f"~/sensor/{object_id}/state",
        "availability_topic": "~/status",
        "device": DEVICE,
    }
    cfg.update(fields)
    return cfg


# object_id, display name, extra discovery fields. mV + tilt are plain (historised)
# sensors; serial + cal_factor are diagnostic (live-only, skipped by the recorder).
# NB: "cal_factor" avoids grow-app's DANGEROUS_WORDS trigger on "calibration".
DISCOVERY_SPECS = [
    ("ppfd", "Canopy PPFD", {"unit_of_measurement": PPFD_UNIT, "state_class": "measurement",
                             "suggested_display_precision": 0, "icon": "mdi:white-balance-sunny"}),
    ("detector_mv", "Detector signal", {"unit_of_measurement": "mV", "state_class": "measurement",
                                        "suggested_display_precision": 3, "icon": "mdi:sine-wave"}),
    ("tilt", "Sensor tilt", {"unit_of_measurement": "°", "state_class": "measurement",
                             "suggested_display_precision": 1, "icon": "mdi:angle-acute"}),
    ("sensor_serial", "Sensor serial", {"entity_category": "diagnostic", "icon": "mdi:identifier"}),
    ("cal_factor", "Cal factor", {"entity_category": "diagnostic", "suggested_display_precision": 5,
                                  "icon": "mdi:function-variant"}),
]


def publish_discovery(client):
    for object_id, name, fields in DISCOVERY_SPECS:
        topic = f"{DISCOVERY_PREFIX}/sensor/{NODE_ID}/{object_id}/config"
        client.publish(topic, json.dumps(discovery_config(object_id, name, **fields)), qos=1, retain=True)
    log("published discovery for", ", ".join(s[0] for s in DISCOVERY_SPECS))


def publish_state(client, object_id, value):
    client.publish(f"{BASE}/sensor/{object_id}/state", value, qos=1, retain=True)


def make_client():
    client_id = f"apogee-{NODE_ID}"
    try:
        return mqtt.Client(mqtt.CallbackAPIVersion.VERSION2, client_id=client_id)
    except (AttributeError, TypeError):
        return mqtt.Client(client_id=client_id)  # paho-mqtt 1.x


def open_sensor(retries=12, delay=3.0):
    """Open the SDI-12 adapter, retrying — USB enumeration can lag a Pi reboot."""
    for attempt in range(1, retries + 1):
        try:
            return Sdi12(SDI12_PORT, SDI12_ADDRESS)
        except (serial.SerialException, OSError) as exc:
            log(f"serial open failed ({attempt}/{retries}): {exc}")
            time.sleep(delay)
    raise SystemExit(f"could not open {SDI12_PORT} after {retries} attempts")


def main():
    stop = {"flag": False}

    def _stop(*_):
        stop["flag"] = True

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    sdi = open_sensor()
    ident = sdi.identify()
    log("identity:", repr(ident))
    fw, serial_no = parse_identity(ident)
    if fw:
        DEVICE["sw_version"] = fw
    cal_factor = None
    try:
        cal_factor = sdi.verify()  # first V! coefficient = calibration multiplier
    except Exception as exc:  # noqa: BLE001 - best-effort, never fatal
        log("cal factor read failed:", exc)
    log(f"firmware={fw} serial={serial_no} cal_factor={cal_factor}")

    statics = {}
    if serial_no:
        statics["sensor_serial"] = serial_no
    if cal_factor is not None:
        statics["cal_factor"] = f"{cal_factor:.6g}"

    def on_connect(client, userdata, flags, reason_code, properties=None):
        failed = getattr(reason_code, "is_failure", None)
        if failed is None:
            failed = reason_code != 0
        if failed:
            log("MQTT connect failed:", reason_code)
            return
        log("MQTT connected")
        publish_discovery(client)
        client.publish(STATUS_TOPIC, "online", qos=1, retain=True)
        for object_id, value in statics.items():
            publish_state(client, object_id, value)

    client = make_client()
    if MQTT_USERNAME:
        client.username_pw_set(MQTT_USERNAME, MQTT_PASSWORD)
    client.will_set(STATUS_TOPIC, "offline", qos=1, retain=True)
    client.on_connect = on_connect
    # connect_async + loop_start so a broker that's down at boot doesn't crash the process — the
    # network thread keeps retrying and fires on_connect (discovery + online) once it comes up.
    client.connect_async(MQTT_HOST, MQTT_PORT, keepalive=30)
    client.loop_start()

    # object_id, SDI-12 measurement sub-command, display precision
    readings = [("ppfd", "", 1), ("detector_mv", "1", 4), ("tilt", "4", 2)]

    try:
        while not stop["flag"]:
            try:
                values = {}
                for object_id, sub, precision in readings:
                    value = sdi.measure(sub)
                    if value is None:
                        log(f"no reading for {object_id}")
                        continue
                    publish_state(client, object_id, f"{value:.{precision}f}")
                    values[object_id] = value
                if "ppfd" in values:
                    log(f"PPFD={values['ppfd']:.1f} umol  mV={values.get('detector_mv')}  tilt={values.get('tilt')}")
            except (serial.SerialException, OSError) as exc:
                log("serial error, reopening adapter:", exc)
                sdi.close()
                time.sleep(2)
                try:
                    sdi.open()
                except Exception as exc2:  # noqa: BLE001
                    log("adapter reopen failed:", exc2)

            slept = 0.0
            while slept < POLL_SECONDS and not stop["flag"]:
                step = min(0.5, POLL_SECONDS - slept)
                time.sleep(step)
                slept += step
    finally:
        log("shutting down")
        try:
            client.publish(STATUS_TOPIC, "offline", qos=1, retain=True)
            time.sleep(0.3)
        except Exception:
            pass
        client.loop_stop()
        try:
            client.disconnect()
        except Exception:
            pass
        sdi.close()


if __name__ == "__main__":
    main()

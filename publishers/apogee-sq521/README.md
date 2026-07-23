# Apogee SQ-521 → MQTT publisher

A standalone Python service that reads an **Apogee SQ-521** full-spectrum quantum
(PAR) sensor over SDI-12 and publishes it to the `grow/daniel-home` MQTT bus in the
same Home-Assistant-discovery style the ESPHome fleet uses. grow-app auto-registers
the device and `grow-history-recorder` captures it — **no grow-app or broker changes
are required**.

It runs on **quantum-sensor.home.arpa** (a Raspberry Pi 5), not in the media-stack
Docker host, because the sensor is physically wired to that Pi.

## Hardware / wiring

Apogee SQ-521 (SDI-12) → genuine Apogee cable → **Dr. Liu SDI-12→USB adapter** →
Pi USB (enumerates as an FTDI port, `/dev/ttyUSB0`).

Cable colours (SQ-521 manual p.12):

| Wire  | Function                         | Adapter terminal |
|-------|----------------------------------|------------------|
| Red   | Input power (5.5–24 V)           | `+`              |
| White | SDI-12 data ("positive signal")  | `S`              |
| Black | Ground                           | `−`              |
| Clear | Cable shield/ground (optional)   | `−`              |

## What it publishes

Device `quantum-sensor` (Apogee Instruments SQ-521):

| Entity          | object_id       | Unit       | InfluxDB? | Notes                                  |
|-----------------|-----------------|------------|-----------|----------------------------------------|
| Canopy PPFD     | `ppfd`          | µmol/s/m²  | yes       | primary; drives the Light-page card    |
| Detector signal | `detector_mv`   | mV         | yes       | raw detector millivolts                |
| Sensor tilt     | `tilt`          | °          | yes       | angle from vertical (serial ≥ 3033)    |
| Sensor serial   | `sensor_serial` | —          | no        | static, diagnostic                     |
| Cal factor      | `cal_factor`    | —          | no        | static, diagnostic (cal multiplier)    |

`ppfd`/`detector_mv`/`tilt` are plain (non-diagnostic) sensors, so the recorder
writes them to InfluxDB (measurement `reading`, tags `site`/`node`/`entity`/
`component`/`unit`). The two static values carry `entity_category: diagnostic`, so
they show live in grow-app but are excluded from history.

Topics (all retained, discovery/state qos 1):

- Discovery: `grow/daniel-home/_discovery/sensor/quantum-sensor/<object_id>/config`
- State (plain scalar): `grow/daniel-home/quantum-sensor/sensor/<object_id>/state`
- Availability (LWT): `grow/daniel-home/quantum-sensor/status` → `online`/`offline`

## SDI-12 command map (SQ-521, address 0)

| Command       | Returns                                   |
|---------------|-------------------------------------------|
| `0M!` / `0D0!`  | PPFD, µmol·m⁻²·s⁻¹                       |
| `0M1!` / `0D0!` | detector output, mV                     |
| `0M4!` / `0D0!` | tilt from vertical, ° (serial ≥ 3033)   |
| `0V!` / `0D0!`  | calibration coefficients (multiplier …) |
| `0I!`           | identity (vendor, model, firmware, serial) |

The Dr. Liu adapter is a 9600-8N1 serial port; opening it resets the adapter, so the
publisher waits ~2.5 s after opening before the first command.

## Install (on the Pi)

```bash
# 1. Code + deps (reuses the existing venv that already has pyserial)
mkdir -p ~/apogee-sq521
cp apogee_publisher.py requirements.txt config.example.env ~/apogee-sq521/
~/.venvs/sdi12/bin/pip install -r ~/apogee-sq521/requirements.txt

# 2. Config — set MQTT_PASSWORD to the MQTT_EDGE_PASSWORD value from media-stack
cp ~/apogee-sq521/config.example.env ~/apogee-sq521/config.env
chmod 600 ~/apogee-sq521/config.env
nano ~/apogee-sq521/config.env

# 3. One-off smoke test (Ctrl-C to stop)
set -a; . ~/apogee-sq521/config.env; set +a
~/.venvs/sdi12/bin/python ~/apogee-sq521/apogee_publisher.py

# 4. Run it as a service
sudo cp apogee-sq521.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now apogee-sq521
journalctl -u apogee-sq521 -f
```

## Verify it landed on the bus

```bash
mosquitto_sub -h 192.168.8.3 -p 1883 -u edge-daniel-home -P "$MQTT_PASSWORD" -v \
  -t 'grow/daniel-home/quantum-sensor/#' \
  -t 'grow/daniel-home/_discovery/sensor/quantum-sensor/#'
```

You should see the five retained discovery configs plus live `.../ppfd/state`
scalars. In grow-app, the **Canopy PPFD** card on the Lights page switches to
`MEASURED` (green, no `≈`), and the spectrometer's Calibration tab gains an
**"Anchor from live Apogee"** button.

## Notes

- **Tilt** (`0M4!`) needs an SQ-521 with serial number ≥ 3033. On older units the
  publisher just logs "no reading for tilt" and skips it — remove `tilt` from the
  `readings`/`DISCOVERY_SPECS` lists (and clear its retained discovery topic) if you
  don't want the empty entity.
- **Cal factor** is read once at startup via `0V!`; if that parse fails it's simply
  omitted — it never blocks PPFD.
- **PPFD unit** is configurable via `PPFD_UNIT`; grow-app matches the sensor on
  `object_id: "ppfd"`, so the exact unit spelling only affects the InfluxDB tag and
  the generic device display, not the Light-page card.
- Credential: this reuses the shared `edge-daniel-home` account, whose
  `readwrite grow/daniel-home/#` grant already spans both the device and `_discovery`
  subtrees. To scope a dedicated `quantum-sensor` user later you'd need a second ACL
  grant for `_discovery` (it lives outside the device subtree).

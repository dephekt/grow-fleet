# Arduino fleet firmware

Non-ESPHome fleet devices, built with `arduino-cli` and published as the **same**
`grow-firmware-package.v1` OCI artifacts as the ESPHome devices, so grow-app serves
them unchanged. Registered under `arduino_devices:` in `fleet.yaml`. The ESPHome
`devices:` map and `firmware.yml` are untouched — this is a parallel path that reuses
`scripts/{fleetlib,publish_packages}.py`.

## Devices

- **spectrometer** — Hamamatsu C12880MA on an Arduino UNO R4 WiFi
  (`fqbn arduino:renesas_uno:unor4wifi`). Reads the 288-px spectrum, publishes raw
  counts to `grow/daniel-home/spectrometer/spectrum/state`, and self-updates over OTA.

## Build / package / publish

`scripts/build_arduino.py` does `arduino-cli compile → lzss → bin2ota → dist/<device>/
<device>.ota.bin + <device>.manifest.json → oras push ghcr.io/dephekt/grow-fleet-<device>:<version>`.

```sh
# local build + package (no publish); needs devices/secrets.yaml + arduino-cli on PATH
python3 scripts/build_arduino.py spectrometer --version v0.0.1 \
  --source-sha "$(git rev-parse HEAD)" --build-profile site-private

# publish (needs `oras login ghcr.io` first, GHCR token with write:packages)
python3 scripts/build_arduino.py spectrometer --version v0.0.1 \
  --source-sha "$(git rev-parse HEAD)" --build-profile site-private \
  --require-flashable-secrets --publish
```

CI (`.github/workflows/arduino-firmware.yml`): `push` to `main` → edge publish;
tag `arduino-firmware/<device>/vX.Y.Z` → stable publish; PRs compile-only. Needs the
same secrets as `firmware.yml`: `FLEET_SECRETS_YAML_B64`, `GHCR_USER`, `GHCR_TOKEN`.

## OTA image format (the `.ota`)

The UNO R4 WiFi `OTAUpdate` library applies an **LZSS-compressed** image wrapped with a
bin2ota header — `[len u32][crc32 u32][magic u32][version 8B, byte7=0x40][compressed]`.
`build_arduino.py` runs `tools/lzss.py --encode` then `tools/mkota.py UNOR4WIFI` (a
stdlib re-impl of Arduino's `bin2ota.py`, no `crccheck` dep). Feeding a raw (uncompressed)
binary produces a flagged-but-uncompressed image the modem unpacks into garbage — it MUST
be LZSS-compressed first.

## Secrets

`build_arduino.py` generates `arduino/<device>/arduino_secrets.h` (gitignored) from the
fleet `secrets.yaml` at build time: `wifi_ssid/wifi_password/mqtt_password/firmware_update_token`
plus the constant MQTT user `edge-daniel-home` and broker `192.168.8.3`. A committed
`arduino_secrets.h.example` documents the shape. `fw_version.h` (gitignored) carries the
`FW_VERSION` string.

## UNO R4 WiFi gotchas (learned the hard way)

- **OTA needs modem (ESP32-S3) firmware ≥ 0.3.0.** If OTAUpdate `begin()` returns `-26`
  (Modem error), the ESP32-S3 firmware is too old. Update it (once). If `arduino-fwuploader`'s
  espflash can't reset the ESP32-S3 on your machine (DTR "protocol error"), short **GND+Download**
  on the 6-pin header by the USB-C to force ROM download mode (`303a:1001`), then:
  `espflash write-bin --before no-reset --after no-reset --baud 115200 -p /dev/ttyACMx 0x0 ESP32-S3.bin`.
  This also fixes flaky uploads.
- **Reading serial**: use `arduino-cli monitor -p /dev/ttyACMx --config baudrate=115200` — a bare
  `cat` doesn't assert DTR and returns nothing.
- **Wedged board recovery**: power-cycle → double-tap RESET (amber breathing LED) →
  `bossac -d --port=ttyACMx -U -e -w <fw>.bin -R` directly (skips the broken 1200-bps touch).

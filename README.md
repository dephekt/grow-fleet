# grow-fleet

ESPHome firmware configurations for the stackdrift grow controller fleet.

This repository owns real device YAMLs, firmware compile CI, and release
artifacts for site-local OTA workflows. Reusable ESPHome components remain in
[`dephekt/esphome-components`](https://github.com/dephekt/esphome-components).

## CI Tooling

The CI slice in this repo is driven by the scripts under `scripts/` and the
GitHub Actions workflow at `.github/workflows/firmware.yml`.

Common local commands:

```sh
make list-devices
make build atoms3u-sensor-rig
make flash atoms3u-sensor-rig PORT=/dev/ttyACM0
make logs atoms3u-sensor-rig PORT=/dev/ttyACM0
uv run --locked python scripts/list_devices.py
uv run --locked python scripts/impacted_devices.py --base <base-sha> --head <head-sha>
uv run --locked python scripts/compile_devices.py --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache uv run --locked python scripts/cache_firmware.py store --sha <sha> --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache uv run --locked python scripts/cache_firmware.py restore --sha <sha> atlas-hydro-kit
uv run --locked python scripts/package_device.py atlas-hydro-kit --version v1.2.3
uv run --locked python scripts/publish_packages.py atlas-hydro-kit
```

The Makefile targets run ESPHome through `./docker/esphome`, a Docker Compose
wrapper that forces the local Docker context so USB devices such as
`/dev/ttyACM0` are visible even when the default Docker context points at a
remote host. The direct Python compile path still works in CI or on hosts with
ESPHome installed; set `ESPHOME=./docker/esphome` to route it through Docker.
`make flash` checks that `devices/secrets.yaml` exists and is not using the
compile-only placeholder values before flashing.

The release workflow packages compiled firmware as `dist/<device>/<device>.ota.bin`,
`dist/<device>/<device>.factory.bin`, and `dist/<device>/<device>.manifest.json`.
Publishing uses private GHCR OCI artifacts via `oras`. Log in to GHCR before
running `scripts/publish_packages.py`; the default package names are
`ghcr.io/dephekt/grow-fleet-<device>`. Keep those firmware packages
private; the public repository is only for source, structure, and config.

Workflow behavior:

- Pull requests compile only impacted devices.
- Pull requests and manual dispatches use compile-only placeholder secrets and
  never publish firmware.
- Pushes to `main` compile changed release devices with protected firmware
  secrets, publish edge packages to private GHCR OCI artifacts, and prune old
  edge tags.
- Tags matching `firmware/<device>/vX.Y.Z` compile, package, and publish that
  one stable firmware package.
- If `FLEET_SECRETS_YAML_B64` is not configured yet, trusted publish jobs skip
  the protected firmware build instead of failing the first migration push.

GitHub-hosted runners cache the PlatformIO core directory with GitHub Actions
cache to reuse downloaded platforms, packages, and toolchains between runs.
They do not cache `devices/.esphome/build`: protected firmware builds embed
site secrets in the compiled binaries. Private GHCR OCI artifacts are the
durable release output.

## License

Copyright (c) 2026 Daniel Snider.

Licensed under the **GNU Affero General Public License, version 3 or later**
(AGPL-3.0-or-later). The full text is in [LICENSE](LICENSE). Contributions
require an explicit license grant beyond the AGPL — see
[CONTRIBUTING.md](CONTRIBUTING.md).

### Third-party code

| Path | Origin | License |
|---|---|---|
| `arduino/tools/lzss.c` | Haruhiko Okumura's LZSS codec, redistributed by Arduino | Public domain |
| `arduino/tools/lzss.py` | Arduino OTA tooling | Unresolved — no upstream header |
| `arduino/tools/bin2ota.py` | Arduino OTA tooling | Unresolved — no upstream header |

These files are **excluded** from this repository's AGPL grant and retain their
own terms. See [`arduino/tools/README.md`](arduino/tools/README.md) for detail
and for how to drop the unresolved dependency. Everything else in `arduino/`,
including `mkota.py`, is original work under the AGPL.

The ESPHome components these device configs pull in live in
[`dephekt/esphome-components`](https://github.com/dephekt/esphome-components)
under a dual GPL-3.0-or-later / MIT license. Device YAMLs reference those
components rather than deriving from them, so the two licenses do not interact.

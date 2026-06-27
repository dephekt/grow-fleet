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
uv run --locked python scripts/list_devices.py
uv run --locked python scripts/impacted_devices.py --base <base-sha> --head <head-sha>
uv run --locked python scripts/compile_devices.py --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache uv run --locked python scripts/cache_firmware.py store --sha <sha> --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache uv run --locked python scripts/cache_firmware.py restore --sha <sha> atlas-hydro-kit
uv run --locked python scripts/package_device.py atlas-hydro-kit --version v1.2.3
uv run --locked python scripts/publish_packages.py atlas-hydro-kit
```

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

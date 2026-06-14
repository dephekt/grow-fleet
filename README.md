# grow-fleet

ESPHome firmware configurations for the stackdrift grow controller fleet.

This repository owns real device YAMLs, firmware compile CI, and release
artifacts for site-local OTA workflows. Reusable ESPHome components remain in
[`stackdrift/esphome-components`](https://codeberg.org/stackdrift/esphome-components).

## CI Tooling

The CI slice in this repo is driven by the scripts under `scripts/` and the
Forgejo workflow at `.forgejo/workflows/firmware.yml`.

Common local commands:

```sh
python3 scripts/list_devices.py
python3 scripts/impacted_devices.py --base <base-sha> --head <head-sha>
python3 scripts/compile_devices.py --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache python3 scripts/cache_firmware.py store --sha <sha> --all
FIRMWARE_CACHE_ROOT=/tmp/grow-fleet-cache python3 scripts/cache_firmware.py restore --sha <sha> atlas-hydro-kit
python3 scripts/package_device.py atlas-hydro-kit --version v1.2.3
python3 scripts/publish_packages.py atlas-hydro-kit
```

The release workflow packages compiled firmware as `dist/<device>/<device>.ota.bin`,
`dist/<device>/<device>.factory.bin`, and `dist/<device>/<device>.manifest.json`.
Publishing uses the Forgejo generic package API with `PACKAGE_TOKEN` or
`FORGEJO_TOKEN`, uses Basic auth for `PACKAGE_TOKEN`, uses Bearer auth for the
automatic `FORGEJO_TOKEN`, and defaults to the `stackdrift` package namespace.
`PACKAGE_AUTH_USER` only applies to `PACKAGE_TOKEN` publishing.

Workflow behavior:

- Pull requests compile only impacted devices.
- Pull requests and manual dispatches share the runner-local PlatformIO cache.
- Pushes to `main` compile every device, store compiled firmware under the runner-local cache keyed by commit SHA, and prune old cached firmware.
- Tags matching `firmware/<device>/vX.Y.Z` restore cached firmware for the tagged commit when available, falling back to compile/store on a miss, then package and publish that one device.

Firmware cache storage is runner-local at `/runner-cache/grow-fleet`, backed by
`/srv/forgejo-runner/cache/grow-fleet` on the runner host. The workflow does not
use Codeberg Actions artifacts for this temporary cache; Forgejo generic
packages remain the durable release output.
The runner host must create that directory and include it in the runner
`container.valid_volumes` allowlist; otherwise the runner ignores the bind mount
before the job starts.

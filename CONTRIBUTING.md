# Contributing

Issues and pull requests are welcome.

## Licensing of contributions

grow-fleet is licensed under the **GNU Affero General Public License, version 3
or later** (see [LICENSE](LICENSE)), except for the vendored Arduino OTA tooling
under `arduino/tools/` — see [that directory's README](arduino/tools/README.md).

The project is maintained under single-copyright ownership so that alternative
licensing terms remain available to the copyright holder. To keep that
possible, contributions require an explicit grant beyond the AGPL — a sign-off
alone is not sufficient, because code received under the AGPL cannot later be
offered to anyone under different terms.

**By submitting a pull request, patch, or any other contribution to this
repository, you agree to the following:**

1. You are the author of the contribution, or you have the right to submit it
   under these terms.
2. You grant Daniel Snider a perpetual, worldwide, non-exclusive, royalty-free,
   irrevocable license to reproduce, modify, distribute, sublicense, and
   otherwise exploit your contribution, **under the AGPL-3.0-or-later and under
   any other license terms**, including proprietary terms.
3. You retain copyright in your contribution. This is a license grant, not an
   assignment — you may continue to use your own work however you like.
4. Your contribution is provided as-is, without warranty of any kind.

Please add a `Signed-off-by:` line to your commits (`git commit -s`) to record
your agreement, per the
[Developer Certificate of Origin](https://developercertificate.org/).

If you would rather not grant those terms, open an issue describing the change
instead of a pull request.

## Third-party code

Do not add vendored third-party code outside `arduino/tools/`. If a change
genuinely needs it, keep it in its own directory with the upstream license file
intact and add it to the third-party table in the README, rather than inlining
it into `scripts/` or a device YAML.

Note that the ESPHome components these device configs consume live in
[`dephekt/esphome-components`](https://github.com/dephekt/esphome-components)
and are dual GPL-3.0-or-later / MIT. Device YAMLs *reference* those components;
referencing is not derivation, which is why this repo is free to be AGPL.

## New files

Every source file we own carries a two-line SPDX header:

```python
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Daniel Snider
```

Device YAMLs under `devices/` use the same form. Use your own name in the
copyright line for files you author outright.

## Checks

```sh
uv run --locked ruff check .
uv run --locked ruff format --check .
uv run --locked mypy
uv run --locked pytest
```

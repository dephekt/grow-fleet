#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import os
from pathlib import Path

from fleetlib import DEVICE_SECRETS_PATH, assert_flashable_secrets


def install_secrets_from_env(env_name: str, target: Path) -> None:
    encoded = os.environ.get(env_name)
    if not encoded:
        raise SystemExit(f"{env_name} is required")

    compact = "".join(encoded.split())
    try:
        decoded = base64.b64decode(compact, validate=True)
    except ValueError as exc:
        raise SystemExit(f"{env_name} must be base64-encoded YAML") from exc

    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(decoded)
    target.chmod(0o600)


def main() -> None:
    parser = argparse.ArgumentParser(description="Install protected ESPHome secrets for flashable firmware builds.")
    parser.add_argument(
        "--env",
        default="FLEET_SECRETS_YAML_B64",
        help="Environment variable containing base64-encoded secrets.yaml.",
    )
    parser.add_argument(
        "--target",
        default=str(DEVICE_SECRETS_PATH),
        help="Target secrets.yaml path.",
    )
    parser.add_argument(
        "--require-flashable",
        action="store_true",
        help="Reject missing or compile-only placeholder secret values after installation.",
    )
    args = parser.parse_args()

    target = Path(args.target)
    install_secrets_from_env(args.env, target)
    if args.require_flashable:
        assert_flashable_secrets(target)


if __name__ == "__main__":
    main()

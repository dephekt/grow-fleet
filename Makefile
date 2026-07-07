PYTHON ?= uv run --locked python
ESPHOME ?= ./docker/esphome
PORT ?= /dev/ttyACM0
FIRMWARE_VERSION ?=
PACKAGE_OWNER ?=
VERSION ?=
REF ?= HEAD

DEVICE_NAMES := $(shell $(PYTHON) scripts/list_devices.py 2>/dev/null)
DEVICE_GOAL := $(firstword $(filter $(DEVICE_NAMES),$(MAKECMDGOALS)))
DEVICE ?= $(DEVICE_GOAL)

COMPILE_ARGS := $(if $(FIRMWARE_VERSION),--firmware-version $(FIRMWARE_VERSION)) $(if $(PACKAGE_OWNER),--package-owner $(PACKAGE_OWNER))
ESPHOME_SUBS := $(if $(FIRMWARE_VERSION),-s firmware_version $(FIRMWARE_VERSION)) $(if $(PACKAGE_OWNER),-s package_owner $(PACKAGE_OWNER))

.PHONY: help list-devices build flash logs release-tag $(DEVICE_NAMES)

help:
	@printf '%s\n' \
		'Targets:' \
		'  make list-devices' \
		'  make build <device>' \
		'  make flash <device> PORT=/dev/ttyACM0' \
		'  make logs <device> PORT=/dev/ttyACM0' \
		'  make release-tag <device> VERSION=vX.Y.Z' \
		'' \
		'Examples:' \
		'  make build atoms3u-sensor-rig' \
		'  make flash atoms3u-sensor-rig PORT=/dev/ttyACM0' \
		'  make release-tag atoms3u-sensor-rig VERSION=v0.2.1'

list-devices:
	$(PYTHON) scripts/list_devices.py

build:
	@test -n "$(DEVICE)" || { echo "Usage: make build <device> or make build DEVICE=<device>" >&2; exit 2; }
	ESPHOME="$(ESPHOME)" $(PYTHON) scripts/compile_devices.py $(COMPILE_ARGS) "$(DEVICE)"

flash:
	@test -n "$(DEVICE)" || { echo "Usage: make flash <device> PORT=/dev/ttyACM0 or make flash DEVICE=<device>" >&2; exit 2; }
	@PYTHONPATH=scripts $(PYTHON) -c 'from fleetlib import assert_flashable_secrets; assert_flashable_secrets()'
	@config="$$(PYTHONPATH=scripts $(PYTHON) -c 'from fleetlib import ROOT, device_spec; import sys; print(device_spec(sys.argv[1]).config.relative_to(ROOT))' "$(DEVICE)")"; \
		$(ESPHOME) $(ESPHOME_SUBS) upload "$$config" --device "$(PORT)"

logs:
	@test -n "$(DEVICE)" || { echo "Usage: make logs <device> PORT=/dev/ttyACM0 or make logs DEVICE=<device>" >&2; exit 2; }
	@config="$$(PYTHONPATH=scripts $(PYTHON) -c 'from fleetlib import ROOT, device_spec; import sys; print(device_spec(sys.argv[1]).config.relative_to(ROOT))' "$(DEVICE)")"; \
		$(ESPHOME) $(ESPHOME_SUBS) logs "$$config" --device "$(PORT)"

# Cut a stable release tag. Tags MUST be lightweight (unsigned) and pushed one
# at a time: this repo's global git config force-signs tags, and GitHub Actions
# does not trigger the release workflow for signed-annotated tags or for several
# tags pushed together. This target sidesteps both.
release-tag:
	@test -n "$(DEVICE)" || { echo "Usage: make release-tag <device> VERSION=vX.Y.Z" >&2; exit 2; }
	@test -n "$(VERSION)" || { echo "Usage: make release-tag <device> VERSION=vX.Y.Z" >&2; exit 2; }
	@printf '%s' "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION must be vX.Y.Z (got '$(VERSION)')" >&2; exit 2; }
	@test -n "$(filter $(DEVICE),$(DEVICE_NAMES))" || { echo "unknown device: $(DEVICE)" >&2; exit 2; }
	@tag="firmware/$(DEVICE)/$(VERSION)"; \
		if git rev-parse -q --verify "refs/tags/$$tag" >/dev/null 2>&1; then \
			echo "tag already exists locally: $$tag" >&2; exit 2; \
		fi; \
		git -c tag.gpgsign=false tag "$$tag" "$(REF)" && \
		git push origin "$$tag" && \
		echo "pushed lightweight tag $$tag -> $$(git rev-parse --short "$$tag^{commit}")"

$(DEVICE_NAMES):
	@:

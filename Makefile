PYTHON ?= uv run --locked python
ESPHOME ?= ./docker/esphome
PORT ?= /dev/ttyACM0
FIRMWARE_VERSION ?=
PACKAGE_OWNER ?=

DEVICE_NAMES := $(shell $(PYTHON) scripts/list_devices.py 2>/dev/null)
DEVICE_GOAL := $(firstword $(filter $(DEVICE_NAMES),$(MAKECMDGOALS)))
DEVICE ?= $(DEVICE_GOAL)

COMPILE_ARGS := $(if $(FIRMWARE_VERSION),--firmware-version $(FIRMWARE_VERSION)) $(if $(PACKAGE_OWNER),--package-owner $(PACKAGE_OWNER))
ESPHOME_SUBS := $(if $(FIRMWARE_VERSION),-s firmware_version $(FIRMWARE_VERSION)) $(if $(PACKAGE_OWNER),-s package_owner $(PACKAGE_OWNER))

.PHONY: help list-devices build flash logs $(DEVICE_NAMES)

help:
	@printf '%s\n' \
		'Targets:' \
		'  make list-devices' \
		'  make build <device>' \
		'  make flash <device> PORT=/dev/ttyACM0' \
		'  make logs <device> PORT=/dev/ttyACM0' \
		'' \
		'Examples:' \
		'  make build atoms3u-sensor-rig' \
		'  make flash atoms3u-sensor-rig PORT=/dev/ttyACM0'

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

$(DEVICE_NAMES):
	@:

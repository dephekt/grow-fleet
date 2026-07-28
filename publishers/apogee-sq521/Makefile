# apogee-sq521 — build, check and deploy the SDI-12 -> MQTT publisher.
#
# The root Makefile drives ESPHome firmware; this one drives the one Go daemon
# in the fleet. Same shape: `make help`, guard clauses that exit 2, no magic.

HOST ?= quantum-sensor.home.arpa
SSH ?= ssh
SCP ?= scp
BIN := apogee-sq521
DIST := dist/$(BIN)
# --match keeps the firmware tags out of it. This repo tags firmware as
# firmware/<device>/vX.Y.Z and arduino-firmware/<device>/vX.Y.Z, and a bare
# `git describe --tags` cheerfully stamps this binary with the spectrometer's
# version. With no publisher tag yet, --always falls back to the short sha.
VERSION ?= $(shell git describe --tags --match 'publisher/$(BIN)/v*' --always --dirty 2>/dev/null || echo dev)

# The Pi is a Raspberry Pi 5 running 64-bit Raspberry Pi OS. CGO_ENABLED=0 is
# what makes the artifact a single static file with no glibc pin and no venv —
# which is in turn what lets the unit set ProtectHome=yes.
GOBUILD := CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
	-ldflags '-s -w -X main.version=$(VERSION)'

.PHONY: help build test lint push deploy smoke clean

help:
	@printf '%s\n' \
		'Targets:' \
		'  make build                  static linux/arm64 binary -> $(DIST)' \
		'  make test                   go test -race, as CI runs it' \
		'  make lint                   gofmt -l, go vet, arm64 cross-build' \
		'  make push   [HOST=...]      build, scp, install the binary ONLY (keeps $(BIN).prev)' \
		'  make deploy [HOST=...]      push + restart; refuses unless the Go unit is installed' \
		'  make smoke  [HOST=...]      one probe + one cycle under the unit sandbox, publishes nothing' \
		'' \
		'HOST defaults to $(HOST); push, deploy and smoke need ssh + passwordless sudo there.' \
		'' \
		'A FIRST INSTALL uses push, not deploy. deploy ends in systemctl restart, and' \
		'until the README runbook has stopped the Python publisher and installed the' \
		'Go unit, the unit of this name IS the Python. See README.md, section Deploy.' \
		'' \
		'smoke exits 69 if the sensor is unreachable and 75 if something already holds' \
		'the instance lock — stop the unit first, or read that as "it is up".'

build:
	@mkdir -p dist
	$(GOBUILD) -o $(DIST) ./cmd/$(BIN)

test:
	go test -race -timeout 5m ./...

lint:
	@unformatted="$$(gofmt -l .)"; \
		test -z "$$unformatted" || { echo "gofmt would rewrite:" >&2; echo "$$unformatted" >&2; exit 1; }
	go vet ./...
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...

# push installs the binary and NOTHING else: no unit, no daemon-reload, no
# restart. That is what makes it the right verb during a first install, while
# the unit of this name may still be the Python publisher — installing a file
# executes nothing, and the README's step 6 is what starts the daemon.
#
# rename(2) rather than install(1) over the target: on a redeploy the old binary
# is very likely executing, and replacing its directory entry is the one way to
# swap it without ETXTBSY and without a window where /usr/local/bin/$(BIN) is
# missing. The displaced binary is kept as $(BIN).prev, the rollback in README.
push: build
	$(SCP) $(DIST) $(HOST):/tmp/$(BIN).new
	$(SSH) $(HOST) 'set -eu; \
		test ! -e /usr/local/bin/$(BIN) || sudo cp -a /usr/local/bin/$(BIN) /usr/local/bin/$(BIN).prev; \
		sudo install -m 0755 -o root -g root /tmp/$(BIN).new /usr/local/bin/$(BIN).new; \
		rm -f /tmp/$(BIN).new; \
		sudo mv -f /usr/local/bin/$(BIN).new /usr/local/bin/$(BIN); \
		/usr/local/bin/$(BIN) --version'

# deploy is a REDEPLOY verb: push plus a restart. `restart` is only the right
# thing to do once the Go unit is the installed one, so the guard runs first and
# before anything is copied. Without it, running this against a Pi that still
# has the hand-deployed Python unit (SAME unit name) installs the Go binary,
# restarts the Python, and then prints a perfectly healthy status for the wrong
# process. First installs are the README runbook, which stops the Python first.
#
# The trailing readlink is the same proof the README's step 6 asks for: it is
# the only thing that distinguishes "the new binary is running" from "systemd
# reported success about a process that was already there".
deploy:
	$(SSH) $(HOST) 'set -eu; \
		exec_start="$$(systemctl show -p ExecStart --value $(BIN) 2>/dev/null || true)"; \
		case "$$exec_start" in \
		  *"path=/usr/local/bin/$(BIN) "*) exit 0 ;; \
		  "") echo "make deploy: no $(BIN).service on $(HOST). This is a first install:" >&2 ;; \
		  *) echo "make deploy: $(BIN).service on $(HOST) does not run /usr/local/bin/$(BIN):" >&2; \
		     echo "  $$exec_start" >&2; \
		     echo "restarting it would restart the publisher that is already there." >&2 ;; \
		esac; \
		echo "follow README.md section Deploy, or use: make push HOST=$(HOST)" >&2; \
		exit 2'
	$(MAKE) push
	$(SSH) $(HOST) 'set -eu; \
		sudo systemctl restart $(BIN); \
		sleep 3; \
		systemctl --no-pager --lines=0 status $(BIN); \
		test "$$(sudo readlink /proc/$$(systemctl show -p MainPID --value $(BIN))/exe)" \
		     = /usr/local/bin/$(BIN) \
		  || { echo "make deploy: MainPID is not /usr/local/bin/$(BIN); the restart did not take" >&2; exit 1; }'

# Through systemd-run under the unit's OWN sandbox: the same user, the same
# supplementary group, the same EnvironmentFile, the same StateDirectory and the
# same confinement. A smoke test that passes under a weaker sandbox than the
# service is not a smoke test.
#
# DeviceAllow is the one that earns its keep here. systemd resolves the name
# "char-ttyUSB" against /proc/devices WHEN THE UNIT STARTS, and a single
# DeviceAllow= flips the default policy from open to closed. On a box where
# ftdi_sio has never been loaded the name is absent, systemd drops the rule with
# no journal warning, and the daemon is then denied the tty — while a smoke test
# without the property opens the very same device happily. Verified both ways.
#
# This list is a COPY of the unit's hardening block, not a derivation of it:
# change one and change the other (apogee-sq521.service says the same above its
# hardening section). What it still cannot cover is Type=notify, WatchdogSec and
# Restart=, none of which --once uses.
smoke:
	$(SSH) -t $(HOST) "sudo systemd-run --pty --quiet --collect --uid=daniel \
		--property=SupplementaryGroups=dialout \
		--property='DeviceAllow=char-ttyUSB rw' \
		--property=NoNewPrivileges=yes \
		--property=ProtectSystem=strict \
		--property=ProtectHome=yes \
		--property=PrivateTmp=yes \
		--property=ProtectKernelTunables=yes \
		--property=ProtectKernelModules=yes \
		--property=ProtectKernelLogs=yes \
		--property=ProtectControlGroups=yes \
		--property=RestrictNamespaces=yes \
		--property=RestrictRealtime=yes \
		--property=RestrictSUIDSGID=yes \
		--property=LockPersonality=yes \
		--property=SystemCallArchitectures=native \
		--property=CapabilityBoundingSet= \
		--property=AmbientCapabilities= \
		--property='RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX' \
		--property=MemoryMax=64M \
		--property=EnvironmentFile=/etc/$(BIN)/config.env \
		--property=StateDirectory=$(BIN) \
		--property=StateDirectoryMode=0750 \
		/usr/local/bin/$(BIN) --once"

clean:
	rm -rf dist

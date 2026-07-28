# apogee-sq521

A single static Go binary that reads an **Apogee SQ-521 quantum (PAR) sensor**
over SDI-12 and publishes it to the grow MQTT bus.

It runs on `quantum-sensor.home.arpa` — a Raspberry Pi 5 (arm64, Raspberry Pi
OS) — talking to the sensor through a Dr. Liu SDI-12-to-USB adapter (FTDI
FT231X) on `/dev/serial/by-id/usb-FTDI_FT231X_USB_UART_D30GQUR5-if00-port0`.

It replaces a hand-deployed Python publisher that ran from `~/apogee-sq521`
against a virtualenv in `~/.venvs/sdi12`. There is no interpreter, no venv and
no `pip` any more: one file in `/usr/local/bin`, one env file in `/etc`, one
unit.

**Deploying this is [one ordered procedure](#deploy), and running it out of
order fails silently — every signal reports success while the old publisher
keeps running.** Read that section before running any of it.

---

## What it publishes

Everything is **retained**, at **QoS 1**, under
`grow/daniel-home/quantum-sensor/`.

| Topic (relative to `grow/daniel-home/quantum-sensor`) | What it is |
| --- | --- |
| `status` | `online` / `offline` — **see the behaviour change below**. Also the MQTT will. |
| `sensor/ppfd/state` | Canopy PPFD, µmol/s/m². The reason the device exists. |
| `sensor/detector_mv/state` | Raw detector signal, mV. |
| `sensor/tilt/state` | Sensor tilt, degrees. Optional — absent on units below serial 3033. |
| `sensor/sensor_serial/state` | Sensor serial number (diagnostic). |
| `sensor/cal_factor/state` | Calibration multiplier from `0V!` (diagnostic). |
| `sensor/dli/state` | Daily light integral so far today, mol/m²/d. |
| `sensor/dli_yesterday/state` | Yesterday's completed DLI. |
| `sensor/photoperiod/state` | Hours of light today. |
| `sensor/peak_ppfd/state` | Today's peak PPFD. |
| `sensor/dli_gap/state` | Minutes of today the DLI could not credit (diagnostic). |
| `text/time_zone/state` | The POSIX TZ string in force. Written by grow-app's reconciler on `text/time_zone/command`. |

Discovery configs go to
`grow/daniel-home/_discovery/<component>/quantum-sensor/<object_id>/config`.

`--check-config` prints every one of those topics and the exact JSON that would
be published, without touching the sensor or the broker. Use it as the reference
rather than this table.

> `text/time_zone/state` carries an **empty** payload until grow-app pushes a
> zone, so a fresh install shows no retained message on it at all. That is
> correct, not a fault.

---

## The one behaviour change from the Python — read this before deploying

**The Python stayed `online` for as long as its process was alive, whatever the
sensor was doing. This daemon does not.**

* After `OFFLINE_AFTER` consecutive failed measurement cycles (3 by default) it
  publishes a retained `offline` to the status topic.
* On the **first** failed cycle it publishes an **empty** retained payload to
  every polled reading — `ppfd`, `detector_mv`, `tilt` — which deletes the
  retained value from the broker rather than leaving a stale number standing.

The status topic has therefore changed meaning:

| | Python | Go |
| --- | --- | --- |
| `online` means | the publisher process is running | **the reading you are looking at is live** |

**grow-app will drop out of `MEASURED` back to its estimate during a sensor
outage.** That is the correct behaviour and it is exactly what grow-app's
staleness handling was written to defend against — grow-app has no time-based
staleness of its own, so a retained scalar left on the broker is served as live
for ever. But it *looks* like a regression to anyone who does not know, so:
**a sensor outage will now visibly change the dashboard.**

Statics (`sensor_serial`, `cal_factor`) and the DLI family are **not** blanked —
they were not measured this cycle and are still true. Here is a real outage,
captured with the sensor silent:

```
grow/daniel-home/quantum-sensor/status offline
grow/daniel-home/quantum-sensor/sensor/cal_factor/state 1.02345
grow/daniel-home/quantum-sensor/sensor/sensor_serial/state 3043
grow/daniel-home/quantum-sensor/sensor/dli/state 0.00
grow/daniel-home/quantum-sensor/sensor/dli_yesterday/state 0.00
grow/daniel-home/quantum-sensor/sensor/peak_ppfd/state 156.9
grow/daniel-home/quantum-sensor/sensor/photoperiod/state 0.001
grow/daniel-home/quantum-sensor/sensor/dli_gap/state 0.00
```

`ppfd`, `detector_mv` and `tilt` are simply gone from the retained set.

---

## Deploy

**Prerequisites on the Pi.** Raspberry Pi OS ships neither, and both are used
below — install them first rather than discovering it halfway through:

```sh
sudo apt-get install -y jq mosquitto-clients
```

The daemon itself needs nothing: it is a static binary, which is the whole point
of the rewrite. These two are for *you*, verifying the download and reading the
bus while you are already on the box.

**Read this whole section before running any of it, then run it in order.** This
is the only deployment path — installing onto a Pi that has never run the Python
and cutting over from one that is running it are the same seven steps, because
the Python uses the same unit name and there is only ever one unit.

Two things make the order load-bearing, and both of them fail **silently**:

* **`systemctl start` on an already-active unit is a no-op.** It does not
  re-exec the new `ExecStart`. Overwrite the unit file (step 4) without stopping
  the Python (step 0) and every signal reports success: `systemctl status` says
  `active (running)`, `systemctl show` echoes the *new* `ExecStart` back at you —
  with `pid=0` and `start_time=[n/a]` buried in the same line — and
  `journalctl -f` sits silent because the Python has nothing new to say. The Go
  daemon never ran. Step 6 uses `restart` and then proves which binary is on the
  end of the MainPID, for exactly this reason.
* **`--once` (step 5) drives the sensor.** It publishes nothing, but it opens
  the tty and runs a full SDI-12 probe and measurement cycle. Running it while
  the Python is polling puts two processes on one SDI-12 bus, and neither
  `flock` nor `TIOCEXCL` stops pyserial — see
  [Why the order matters](#why-the-order-matters).

If the Pi has no checkout of this repo, send the two files that steps 2 and 4
install across first:

```sh
scp publishers/apogee-sq521/{config.example.env,apogee-sq521.service} \
    quantum-sensor.home.arpa:
```

### 0. Stop the Python, and keep a way back

Do this before anything else. The backup has to be taken while the Python's unit
file still exists — step 4 overwrites it — and the Python has to be stopped
before step 5 touches the sensor.

```sh
# the ONLY thing that creates the file Rollback restores from.
# Guarded, because re-running this runbook from the top is the normal recovery
# move: on a second pass the unit at that path is already the Go one, and a bare
# cp would silently overwrite the Python backup with it. cp exits 0 and prints
# nothing either way, so Rollback would then reinstall the Go unit, report a
# healthy apogee-sq521.service, and leave you believing you were back on the
# Python. Never replace this with a bare cp.
test -e ~/apogee-sq521.service.python.bak || \
  sudo cp /etc/systemd/system/apogee-sq521.service ~/apogee-sq521.service.python.bak

sudo systemctl stop apogee-sq521
systemctl is-active apogee-sq521          # prints "inactive", exits 3
```

On a Pi that has never run the Python both commands fail, and both failures are
fine — carry on:

```
cp: cannot stat '/etc/systemd/system/apogee-sq521.service': No such file or directory
Failed to stop apogee-sq521.service: Unit apogee-sq521.service not loaded.
```

The Python's `~/apogee-sq521` tree and `~/.venvs/sdi12` are untouched by any of
this. Nothing below removes them.

### 1. Get the binary

Published as a private GHCR OCI artifact by `.github/workflows/publisher.yml`,
in the same shape as the firmware packages.

```sh
# once: a GitHub PAT with read:packages
printf '%s' "$GHCR_TOKEN" | oras login ghcr.io -u "$GHCR_USER" --password-stdin

oras repo tags ghcr.io/dephekt/grow-fleet-apogee-sq521
```

A push to `main` that touches `publishers/**` (or `scripts/package_publisher.py`,
`scripts/publish_packages.py`, `scripts/fleetlib.py`, or the workflow itself)
publishes `edge-<YYYYMMDDTHHMMSSZ>-<12-char sha>`. Those sort chronologically,
so the newest edge build is the last `edge-*` line.

**If that command failed**, it reports three different things and they mean
different things:

| What the registry says | What it means | What to do |
| --- | --- | --- |
| `name unknown: repository name not known to registry` | Your credential works and the package **does not exist**. Nothing has published it yet: the workflow's `paths` filter did not match the merge, or it ran and skipped login and publish with `::notice::Build-only (no GHCR_USER/GHCR_TOKEN, or not a push event)` because the repo secrets are unset. | Look at the workflow run for the merge commit. If it says `Build-only`, nothing was pushed to the registry at all — use the workstation path below. |
| `unauthorized: authentication required` | You are anonymous **and the package exists**. Anonymous against a package that does *not* exist gives `denied` instead, so this message is evidence the package is there. | Re-run the `oras login` above. |
| `denied: requested access to the resource is denied` | The registry will not say whether it exists, because your credential is not allowed to look — no login, an expired login, or a PAT without `read:packages`. | Re-issue the PAT with `read:packages` and log in again. |

Then pull the tag you picked:

```sh
mkdir -p /tmp/apogee && cd /tmp/apogee
oras pull ghcr.io/dephekt/grow-fleet-apogee-sq521:<tag>

# the manifest carries the digests; check them before installing  (needs jq, bash)
sha256sum -c <(jq -r '.sha256 | to_entries[] | "\(.value)  \(.key)"' apogee-sq521.manifest.json)
# -> apogee-sq521-linux-arm64: OK

# keep the binary you are replacing, if there is one — `install` overwrites it.
# Only `make push` creates .prev automatically; this route does not, and it is
# the route you must use from the Pi itself. Without this line, "Back to the
# previous Go build" below fails with `cp: cannot stat` at the moment you need it.
test -e /usr/local/bin/apogee-sq521 && \
  sudo cp -a /usr/local/bin/apogee-sq521 /usr/local/bin/apogee-sq521.prev

sudo install -m 0755 apogee-sq521-linux-arm64 /usr/local/bin/apogee-sq521
/usr/local/bin/apogee-sq521 --version
```

**Or, from a workstation checkout**, skipping the registry entirely — and the
only option until the first publish has succeeded:

```sh
cd publishers/apogee-sq521
make push              # build arm64, scp, install the binary. Restarts nothing.
```

`make push` keeps the binary it displaces as `/usr/local/bin/apogee-sq521.prev`
— that is the rollback below. It installs a file and does nothing else, which is
what makes it safe here, while the unit of this name may still be the Python.

> **`make deploy` is not the install verb.** It restarts the unit, which at this
> point is still the Python's. It refuses to run until the Go unit is the
> installed one; see [Redeploying a new build](#redeploying-a-new-build).

### 2. Configuration

```sh
sudo install -d -m 0755 /etc/apogee-sq521
sudo install -m 0640 -o root -g daniel ~/config.example.env /etc/apogee-sq521/config.env
sudoedit /etc/apogee-sq521/config.env
```

Only one value is mandatory: `MQTT_PASSWORD`. It is the **`MQTT_EDGE_PASSWORD`**
value from the media-stack deployment — it is not in this repo and should never
be. Everything else has a working default; `config.example.env` documents each
one and when to change it.

The daemon **refuses to start** without a password rather than connecting
anonymously. The Python did the opposite: paho retried a failing CONNECT for ever
on its network thread while `publish()` returned success into a queue, so the
logs looked healthy and nothing reached the bus.

### 3. Check the config — before starting anything

```sh
sudo systemd-run --pty --quiet --collect --uid=daniel \
    --property=EnvironmentFile=/etc/apogee-sq521/config.env \
    --property=StateDirectory=apogee-sq521 \
    --property=StateDirectoryMode=0750 \
    /usr/local/bin/apogee-sq521 --check-config
```

Running it through `systemd-run` means the env file is parsed exactly as the
unit will parse it, by the user the unit will run as. It prints the resolved
configuration (password redacted to `<set>`/`<empty>`), every discovery topic and
payload, and the timing section. It touches neither the sensor nor the broker.
**Exit 0** means good; **exit 78** prints every problem at once at the bottom:

```
configuration is invalid:
MQTT_PASSWORD: must not be empty (MQTT_USERNAME is "edge-daniel-home"); the client
would never connect while publishes silently succeeded into its queue
```

### 4. Unit

```sh
sudo install -m 0644 ~/apogee-sq521.service /etc/systemd/system/apogee-sq521.service
sudo systemctl daemon-reload
```

> **This overwrites the Python's unit file** — same name, no backup taken for
> you. Step 0 made the copy that [Rollback](#rollback) restores from. If you
> skipped it, run this **before** the install line above; it will not clobber a
> good backup if step 0 already ran:
>
> ```sh
> test -e ~/apogee-sq521.service.python.bak || \
>   sudo cp /etc/systemd/system/apogee-sq521.service ~/apogee-sq521.service.python.bak
> ```
>
> Once `install` has run there is nothing left to copy.

### 5. Smoke test

`--once` opens the port, runs one identification/capability probe and one
measurement cycle, prints the result, and **publishes nothing at all** — the MQTT
collaborators are replaced with no-ops, so it cannot perturb retained state. It
does drive the SDI-12 bus, which is why step 0 comes first.

```sh
sudo systemd-run --pty --quiet --collect --uid=daniel \
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
    --property=EnvironmentFile=/etc/apogee-sq521/config.env \
    --property=StateDirectory=apogee-sq521 \
    --property=StateDirectoryMode=0750 \
    /usr/local/bin/apogee-sq521 --once
```

or, from a workstation checkout, `make smoke`, which runs exactly that.

**Why the wall of properties.** They are the unit's own sandbox, copied. The one
that earns its keep is `DeviceAllow=char-ttyUSB rw`: systemd resolves the name
`char-ttyUSB` against `/proc/devices` **when the unit starts**, and a single
`DeviceAllow=` flips the default policy from "allow everything" to "allow only
what is listed". On a box where `ftdi_sio` has never been loaded the name is not
in `/proc/devices`, systemd drops the rule with no warning in the journal, and
the result is a unit that is denied the tty. Verified both ways against this
daemon, on a box with no `ftdi_sio`, with `SDI12_PORT` pointed at a tty that does
exist:

```
# without the property — the open succeeds and the run reaches the sensor
app: the sensor could not be reached: no measurement produced a value: sdi12: 0M!: sdi12: read timed out: sdi12: no response within budget

# with it — the same open, denied
app: the sensor could not be reached: app: dial /dev/ttyS0: serialport: open /dev/ttyS0 (/dev/ttyS0): operation not permitted
```

A smoke test run without those properties would have passed and the unit would
then have failed on the same device. Note that both cases still exit **69**: the
exit code does not distinguish them, the message does.

**What this still does not cover:** `Type=notify`, `WatchdogSec`, `Restart=` and
the start limiter. `--once` uses none of them, so a clean smoke test says the
environment, the permissions and the wire are good — not that the service will
stay up. Step 6 is what establishes that.

A healthy run:

```
device        /dev/serial/by-id/usb-FTDI_FT231X_USB_UART_D30GQUR5-if00-port0 -> /dev/ttyUSB0
sw_version    104
sensor_serial 3043
cal_factor    1.02345

ppfd          grow/daniel-home/quantum-sensor/sensor/ppfd/state        156.9 µmol/s/m²
detector_mv   grow/daniel-home/quantum-sensor/sensor/detector_mv/state 0.1147 mV
tilt          grow/daniel-home/quantum-sensor/sensor/tilt/state        1.23 °
```

**Exit codes.** The binary uses one sysexits(3) set across every mode, so a
script can branch on them, and `systemd-run --pty` propagates them unchanged:

| Code | Name | Meaning | What to do |
| --- | --- | --- | --- |
| `0` | — | Clean. `--help` and `--version` also exit 0, on stdout. | Nothing. |
| `69` | `EX_UNAVAILABLE` | The sensor could not be reached: no adapter, nothing usable came back, **or the tty was denied** (read the message). | Check the cable and `ls -l /dev/serial/by-id/`. **This is what "not plugged in" looks like — it is not a broken binary.** For `operation not permitted`, see the `DeviceAllow` note above and in [Troubleshooting](#troubleshooting). |
| `75` | `EX_TEMPFAIL` | Another copy of **this binary** already holds this node's lock. | Something is already running: the service, or another `--once`. It says nothing about the Python. |
| `78` | `EX_CONFIG` | The environment is wrong. Every problem is listed. | Fix `/etc/apogee-sq521/config.env`. The unit will **not** restart on this. |
| `1` | — | An unexpected failure. | Read the message; this one is a bug. |

69 and 1 being different is the point of the smoke step: it is run before either
the hardware or the binary has been established, and a script that cannot tell
them apart cannot establish the first one.

### 6. Start it — and prove the Go binary is what is running

```sh
sudo systemctl enable apogee-sq521
sudo systemctl restart apogee-sq521
```

**`restart`, not `start`.** If anything is still active under this unit name —
the Python, or a daemon you started earlier — `start` returns success and
changes nothing. `restart` always stops what is there and executes the current
`ExecStart`.

Then prove it, because `systemctl status` alone cannot tell you:

```sh
sudo readlink /proc/$(systemctl show -p MainPID --value apogee-sq521)/exe
# -> /usr/local/bin/apogee-sq521      anything else means the old process survived

systemctl status apogee-sq521
journalctl -u apogee-sq521 -f
```

A healthy start looks like this. The first line is the one people do not expect
— on a fresh install there is no state file yet, and saying so is not a fault:

```
level=INFO msg="dli: starting from zero, no usable state file" \
  path=/var/lib/apogee-sq521/dli-state.json day=2026-07-28 \
  reason="no usable state: open /var/lib/apogee-sq521/dli-state.json: no such file or directory"
level=INFO msg=started node_id=quantum-sensor broker=192.168.8.3:1883 \
  device=/dev/serial/by-id/usb-FTDI_FT231X_USB_UART_D30GQUR5-if00-port0 \
  poll_interval=10s cycle_budget=33s offline_after_cycles=3 offline_after_at_most=1m39s
level=INFO msg="watchdog armed" interval=1m0s ping_every=30s tolerance=2m11s
level=INFO msg="mqtt connected" broker=192.168.8.3:1883 client_id=apogee-quantum-sensor
level=INFO msg="serial session open" device="/dev/serial/by-id/... -> /dev/ttyUSB0"
level=INFO msg="sensor identified" vendor=APOGEE model=SQ-521 firmware=104 serial=3043 sdi12_version=13
level=INFO msg="calibration multiplier read" cal_factor=1.02345
level=INFO msg="optional measurement supported" command=0M4! values=1
level=INFO msg="device is available" topic=grow/daniel-home/quantum-sensor/status
```

After that a working daemon is **silent**. It logs on transitions, not on
success — no line per poll, ever. Silence in the journal plus `sensor ok` in
`systemctl status` is the healthy state.

Last, watch the handover land on the bus from a third machine:
[Verify on the bus](#verify-on-the-bus).

---

## Why the order matters

**What is safe to do while the Python is still running:** installing files.
Copying a binary into `/usr/local/bin` (step 1), writing
`/etc/apogee-sq521/config.env` (step 2) and running `--check-config` (step 3)
execute nothing against the sensor and publish nothing to the bus.

**What is not safe is `--once`, and running both daemons at once.** `--once`
publishes nothing — the MQTT collaborators are no-ops, see
`internal/app/modes.go` — but that is a statement about the *bus*, not about the
*wire*. It opens the tty and drives a real probe and measurement cycle.

The daemon's own instance guard cannot save you here. It protects against a
second copy of *this* binary, not against a different program:

* Layer 1 is an abstract unix socket, `@apogee-sq521-quantum-sensor`. A second
  copy of *this* daemon fails here and exits **75 before MQTT is configured at
  all**, so it cannot publish a single byte. That is what makes an accidental
  double-start, or a `--once` run against the live service, harmless.
* Layers 2 and 3 are `flock` and `TIOCEXCL` on the tty, taken at open. They stop
  anything that takes the same locks, but pyserial takes neither unless it was
  constructed with `exclusive=True`.

So the Python and this daemon can both hold the port at once and interleave
SDI-12 transactions — which is the desync this codebase spends most of its
comments preventing — and both write the same retained `status` topic. That is
why step 0 stops the Python before step 5 touches the sensor, and why there is no
"just try the smoke test first".

---

## Verify on the bus

From any machine with `mosquitto-clients`. Every command here needs the broker
password, and nothing exports it for you. On the Pi, read it out of the config
with systemd's own parser — the file is `0640 root:daniel` and the value is
single-quoted, and its syntax is *not* shell, so sourcing it is not the same
thing:

```sh
export MQTT_PASSWORD="$(sudo systemd-run --pipe --quiet --collect \
    --property=EnvironmentFile=/etc/apogee-sq521/config.env \
    /usr/bin/printenv MQTT_PASSWORD)"
```

On any other machine, take it from the media-stack deployment
(`MQTT_EDGE_PASSWORD`). Either way it ends up on the `mosquitto_*` command line
and therefore in `ps` for the duration — fine on the Pi, worth knowing on a
shared box.

The broker is reachable from the whole LAN, so these work equally well from a
workstation — take the password from media-stack there instead. The `systemd-run`
extraction above is only needed when you are *on* the Pi.

```sh
mosquitto_sub -h 192.168.8.3 -u edge-daniel-home -P "$MQTT_PASSWORD" -v \
  -t 'grow/daniel-home/quantum-sensor/#' \
  -t 'grow/daniel-home/_discovery/+/quantum-sensor/+/config'
```

A fresh subscriber immediately receives the whole retained set, then live
updates. Real output from a healthy daemon (discovery filtered out for length):

```
grow/daniel-home/quantum-sensor/status online
grow/daniel-home/quantum-sensor/sensor/ppfd/state 156.9
grow/daniel-home/quantum-sensor/sensor/detector_mv/state 0.1147
grow/daniel-home/quantum-sensor/sensor/tilt/state 1.23
grow/daniel-home/quantum-sensor/sensor/sensor_serial/state 3043
grow/daniel-home/quantum-sensor/sensor/cal_factor/state 1.02345
grow/daniel-home/quantum-sensor/sensor/dli/state 0.00
grow/daniel-home/quantum-sensor/sensor/dli_yesterday/state 0.00
grow/daniel-home/quantum-sensor/sensor/photoperiod/state 0.001
grow/daniel-home/quantum-sensor/sensor/peak_ppfd/state 156.9
grow/daniel-home/quantum-sensor/sensor/dli_gap/state 0.00
```

Just the availability, with a timeout so it exits on its own:

```sh
mosquitto_sub -h 192.168.8.3 -u edge-daniel-home -P "$MQTT_PASSWORD" -v -W 5 \
  -t 'grow/daniel-home/quantum-sensor/status'
```

If you see **only** `status offline` and nothing else, the daemon is running but
has never completed a capability probe — the adapter is absent. Discovery is
deliberately withheld until the sensor has been asked what it supports, because
announcing `tilt` on a pre-3033 unit leaves a retained config whose state topic
never fills.

---

## Redeploying a new build

Once the Go unit is the installed one, a new build is one command from a
workstation checkout:

```sh
cd publishers/apogee-sq521
make deploy            # push + restart + status + prove the new binary is running
```

`make deploy` is a **redeploy** verb, not an install verb: it ends in
`systemctl restart`, and it refuses to run at all unless
`apogee-sq521.service` already has `ExecStart=/usr/local/bin/apogee-sq521`.
Without that guard, running it on a Pi that still has the Python unit would
install the Go binary, restart the *Python*, and print a perfectly healthy status
for the wrong process. First installs go through [Deploy](#deploy).

`make push` is the same thing without the restart, and is what step 1 uses.

---

## Rollback

**Back to the previous Go build** — if `.prev` exists. `make push`/`make deploy`
create it automatically; the registry route creates it only if you ran the
`cp -a` line in the install step. Check first, because the failure is
`cp: cannot stat` at the worst possible moment:

```sh
ls -l /usr/local/bin/apogee-sq521.prev
```

```sh
sudo systemctl stop apogee-sq521
sudo cp -a /usr/local/bin/apogee-sq521.prev /usr/local/bin/apogee-sq521
sudo systemctl start apogee-sq521
/usr/local/bin/apogee-sq521 --version
```

`cp` rather than `mv` so `.prev` survives the rollback and a second attempt
restores the same binary. Only one generation is kept — every `make push`
overwrites `.prev` — so this is not a two-deep history. It is safe
here only because the service is stopped first — `make push` replaces a
*running* binary and has to use `rename(2)`, which cannot fail with `ETXTBSY` and
leaves no window where `/usr/local/bin/apogee-sq521` is missing.

**Back to the Python**, if the whole rewrite has to go. This restores the file
[step 0](#0-stop-the-python-and-keep-a-way-back) saved; nothing else creates it:

```sh
# Check what you are about to restore. If step 0's guard was ever bypassed this
# file can hold the Go unit, and every check below would then report a healthy
# apogee-sq521.service while the Go daemon kept running.
grep -m1 ExecStart ~/apogee-sq521.service.python.bak    # expect the Python, NOT /usr/local/bin

sudo systemctl stop apogee-sq521
sudo cp ~/apogee-sq521.service.python.bak /etc/systemd/system/apogee-sq521.service
sudo systemctl daemon-reload
sudo systemctl restart apogee-sq521

# Same proof step 6 uses, and for the same reason: status reports success about
# whatever is running, not about what you meant to run. Rollback is the path you
# take under time pressure, so it gets the same check rather than less.
sudo readlink /proc/$(systemctl show -p MainPID --value apogee-sq521)/exe
# expect the Python interpreter, e.g. /usr/bin/python3.11
```

The Python's own `~/apogee-sq521` tree and `~/.venvs/sdi12` are untouched by any
of this — nothing in the install removes them. Delete them only once the Go
daemon has run for a few days.

**Retained state belongs to the broker, not to the process that wrote it.**
Anything this daemon blanked stays blank, and a retained `offline` stays
`offline`, until *something* publishes to those topics again. The Python
overwrites them as it polls and connects. If one is left standing anyway, clear
it by hand — an empty retained payload is how MQTT deletes a retained message
(with `MQTT_PASSWORD` exported as in [Verify on the bus](#verify-on-the-bus)):

```sh
mosquitto_pub -h 192.168.8.3 -u edge-daniel-home -P "$MQTT_PASSWORD" -r -n \
  -t 'grow/daniel-home/quantum-sensor/status'
```

---

## Reading `systemctl status`

The `Status:` line is rebuilt every 30 s and answers the questions you actually
have at a terminal, without opening the journal. Four captured from live runs:

```
Status: "PPFD 156.9 µmol/s/m² (2s old) · DLI 0.00 mol · sensor ok · mqtt up"
Status: "PPFD n/a · sensor not answering · mqtt up"
Status: "PPFD 156.9 µmol/s/m² (27s old) · DLI 0.00 mol · sensor ok · mqtt DOWN (last publish 27s ago)"
Status: "PPFD 156.9 µmol/s/m² (2s old) · DLI 0.00 mol · sensor ok · mqtt up · tilt n/a"
```

Read it left to right:

| Token | Meaning |
| --- | --- |
| `PPFD <n> <unit> (<age> old)` | The number that is actually on the bus, and how old it is. `PPFD n/a` means there is no live reading. |
| `DLI <n> mol` | Today's integral so far. **Absent entirely until one cycle has produced one.** |
| `sensor ok` / `sensor not answering` | **The sensor.** Sourced from the failure ladder. |
| `mqtt up` / `mqtt DOWN (...)` | **The broker.** Sourced from the publish path — the only thing in the daemon that observes the link. Read the caveat below before you trust `up`. |
| `<object_id> n/a` | An optional entity this unit does not implement (e.g. `tilt` below serial 3033). |

The second line above is the one a first install on a Pi with no adapter
actually shows, captured verbatim: **no `DLI` token at all**, because no cycle
has produced a value to integrate. Once the daemon has had one working cycle the
same outage reads `PPFD n/a · DLI 0.00 mol · sensor not answering · mqtt up`.

**The sensor token and the mqtt token are independent, and that is the whole
point.** They come from different sources on purpose: when readings stop
appearing in grow-app, the one line you read at 3 a.m. must not affirmatively
clear the component that is broken. A single "healthy" derived from publish
state would say "mqtt up" next to an hours-old reading during a broker outage.

**But `mqtt up` can be stale, and it is stale exactly when it hurts.** The token
reflects the last publish *attempt*. While the sensor is out there is usually
nothing new to publish — the readings are already blank, the availability is
already `offline` — so nothing attempts a publish and nothing observes the link.
Reproduced: with the sensor absent and the broker then killed, the line still
read `PPFD n/a · sensor not answering · mqtt up` 45 s later. The mirror case
works, because a healthy sensor keeps publishing: broker down, sensor fine, and
the line goes to `mqtt DOWN (last publish 27s ago)` on schedule.

**So when the sensor token says `not answering`, do not read the mqtt token at
all.** The journal is authoritative there, and it is immediate:

```
level=WARN msg="mqtt connection lost" broker=192.168.8.3:1883
level=WARN msg="mqtt connection attempt failed" broker=192.168.8.3:1883 \
  error="failed to connect to mqtt://192.168.8.3:1883: dial tcp 192.168.8.3:1883: connect: connection refused" \
  retry_every=5s note="further failures are logged at debug until one succeeds"
```

---

## Timing — the two numbers people get wrong

### `OFFLINE_AFTER` is cycles, not seconds

`OFFLINE_AFTER=3` does **not** mean 30 seconds at `POLL_SECONDS=10`.

A cycle is one SDI-12 transaction per distinct measurement sub-command, and each
transaction is allowed a budget derived from the sensor's declared conversion
time — not from `POLL_SECONDS`. At defaults there are three sub-commands
(`0M!`, `0M1!`, `0M4!`) and the per-cycle **budget is 33 s**. A silent sensor can
spend that whole budget before its cycle counts as failed, and an overrunning
cycle skips poll boundaries rather than catching up. So:

```
worst case = OFFLINE_AFTER × max(POLL_SECONDS, cycle budget)
           = 3 × max(10 s, 33 s)
           = 1m39s
```

**not** `3 × 10 s`. Both numbers are printed for you, in two places, so you never
have to compute this:

* `--check-config`, under `timing`:
  ```
  cycle budget  33s
  offline after 1m39s (3 failed cycles)
  ```
* the startup log line: `cycle_budget=33s offline_after_cycles=3 offline_after_at_most=1m39s`

An operator who computes `30 s`, waits 40 s and sees no `offline` concludes the
daemon is wedged, and restarts a daemon that is working exactly as configured.
In practice an unplugged sensor fails much faster than the ceiling — a read that
times out at `SDI12_READ_TIMEOUT` ends its transaction there — so 1m39s is an
upper bound, not a schedule.

Note also that a daemon started with **no adapter at all** publishes retained
`offline` immediately, before any cycle has run: the pre-probe birth corrects
whatever a previous process left on the status topic. `OFFLINE_AFTER` governs a
sensor that *was* answering and stopped.

### `WatchdogSec` vs. the daemon's own staleness tolerance

Two different clocks. They compose; they do not compete.

| | Owner | Default | What it decides |
| --- | --- | --- | --- |
| **staleness tolerance** | the daemon | `2m11s` | *Whether* to send `WATCHDOG=1` at all |
| **`WatchdogSec`** | the unit | `60s` | How long systemd waits for one before killing us |

The daemon stamps a heartbeat on every poll-loop iteration — **including while it
is backing off waiting for a device that does not exist.** The heartbeat means
"the poll goroutine is scheduling", never "the sensor is answering", because a
watchdog that restarted the process for an unplugged sensor would re-enumerate
the USB adapter every minute and lose the retained state that says what is
wrong.

The tolerance is the sum of the spans the loop may legitimately spend not
stamping, which follow one another rather than overlapping:

```
dial backoff ceiling      30s
2 × POLL_SECONDS          20s
cycle budget              33s
birth budget              48s     (a reconnection births on another goroutine
                                   while holding the publish lock)
                        ------
                        2m11s
```

The health goroutine pings at `WatchdogSec / 2` = **30 s**, which is also the
daemon's own status-refresh cadence, so one lost datagram is survivable and
`systemctl status` never goes more than 30 s stale.

When the poll loop really has parked, the pings stop and systemd sends
`WatchdogSignal` — **SIGABRT**, which Go handles by dumping every goroutine's
stack into the journal before dying. That dump is worth more than any log line.
End to end: about **2m11s + 30 s + 60 s ≈ 3.5 minutes** from park to restart.

Both numbers are on the startup line:
`msg="watchdog armed" interval=1m0s ping_every=30s tolerance=2m11s`.

---

## Where state lives

`StateDirectory=apogee-sq521` in the unit, so **`/var/lib/apogee-sq521/`**,
owned by `daniel`, mode `0750`. systemd creates it; it is the only path the
daemon writes to, and `ProtectSystem=strict` makes everything else read-only.

| File | What it holds |
| --- | --- |
| `timezone` | The POSIX TZ override pushed by grow-app's reconciler. Raw bytes, no newline, no wrapping format — `cat` it, edit it with a text editor. Absent means "no override, use the system zone". |
| `dli-state.json` | The day's light accumulator. JSON, indented, mode `0644`, written atomically (temp → fsync → rename → fsync the directory) because this is an SD card and the power goes off without warning. |

`dli-state.json` persists the **raw** `umolSeconds` accumulator rather than the
published DLI, so restarts do not re-quantise the number and compound the error
across a day. It also records the zone the day was measured under, so a restart
after a timezone change cannot silently append today's seconds to a day computed
on a different basis.

Losing the directory is not a fault. The daemon logs
`no STATE_DIRECTORY granted; the timezone override and the day's DLI will not
survive a restart` once at startup and runs perfectly well without it — you lose
the override and today's integration, nothing else. **Do not** set
`STATE_DIRECTORY` by hand in `config.env`; it points the files somewhere systemd
has neither created nor made writable.

---

## Troubleshooting

**`systemctl status` says `active (running)` and shows the right `ExecStart`,
but nothing in the journal is new** — the old process is still there and
`systemctl start` never re-executed anything. Look at the same `systemctl show`
line again: `pid=0` and `start_time=[n/a]` inside the `ExecStart=` value mean
that command has never been run. Confirm with
`sudo readlink /proc/$(systemctl show -p MainPID --value apogee-sq521)/exe`, then
`sudo systemctl restart apogee-sq521`. See [Deploy](#deploy) step 6.

**`--once` exits 75** — something already holds the instance lock: the service,
or another `--once`. `sudo systemctl stop apogee-sq521` first if you really want
to probe by hand. It is silent about the Python, which takes no lock this binary
can see.

**`--once` exits 69, `lstat: no such file or directory`** — the adapter has not
enumerated. `ls -l /dev/serial/by-id/`; if the path there differs (a replaced
adapter has a different FTDI serial number), update `SDI12_PORT`.

**`could not open the serial device; retrying`, with `permission denied` or
`operation not permitted`** — two candidates. Either `daniel` is not in `dialout`
(`id daniel`), or the unit's `DeviceAllow=char-ttyUSB rw` rule was skipped:
systemd resolves that name against `/proc/devices` **when the unit starts**, and
if `ftdi_sio` had never been loaded the group is not listed, so the rule silently
does not exist and the tty is denied. Check with `grep ttyUSB /proc/devices`; if
it is missing, `sudo modprobe ftdi_sio` and restart the unit, and make it
durable with `echo ftdi_sio | sudo tee /etc/modules-load.d/ftdi_sio.conf`. The
[smoke test](#5-smoke-test) reproduces this only because it applies the same
`DeviceAllow`.

**The unit is `failed` and will not restart** — check the exit code with
`systemctl show apogee-sq521 -p ExecMainStatus`. `78` is a config error and the
unit is deliberately configured not to retry it (`RestartPreventExitStatus=78`);
run `--check-config` and fix what it lists. Anything else has hit
`StartLimitBurst=5` inside `StartLimitIntervalSec=300`; `journalctl -u
apogee-sq521 -n 200` will have the cause, and `systemctl reset-failed
apogee-sq521` clears the limiter.

**Readings stopped but the daemon looks fine** — read the `Status:` line before
the journal. `sensor not answering` and `mqtt DOWN` are separate tokens for
exactly this moment. **If the sensor token says `not answering`, ignore the mqtt
token** — with nothing being published, nothing is observing the broker link and
it will keep saying `mqtt up` through a broker outage. `journalctl -u
apogee-sq521 | grep mqtt` is the truth. See
[Reading `systemctl status`](#reading-systemctl-status).

**`sensor identification failed; keeping whatever a previous session read`** — a
transient `0I!` timeout. The daemon deliberately keeps the previous session's
values rather than retracting `sensor_serial` and `cal_factor`, because a single
timeout must not delete established entities. If it repeats, the adapter or the
cable is the suspect, not the daemon.

**`adapter swallowed data-ready notifications during this session`** — a count
that rises across sessions means the USB bridge is dropping traffic. Re-seat the
adapter; try a different port; suspect the cable.

---

## Development

```sh
cd publishers/apogee-sq521
make            # help
make test       # go test -race, exactly as CI runs it
make lint       # gofmt -l, go vet, arm64 cross-build
make build      # dist/apogee-sq521, static linux/arm64
make push       # build, scp, install the binary — restarts nothing  (HOST=quantum-sensor.home.arpa)
make deploy     # push + restart; refuses unless the Go unit is already installed
make smoke      # --once on the Pi, under the unit's sandbox
```

CI gates live in `.github/workflows/checks.yml` (`gofmt`, `go build`, `go vet`,
`go test -race`, plus a `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` cross-build that
asserts the module stays cgo-free). Publishing lives in
`.github/workflows/publisher.yml`, which runs only on a push to `main` under a
`paths` filter and skips the registry entirely when `GHCR_USER`/`GHCR_TOKEN` are
unset.

`make smoke`'s property list is a **copy** of the unit's hardening block, not a
derivation of it. Change one and change the other; the unit file says so too.

The architecture, the orderings the daemon enforces and the reasoning behind each
are documented in the package comments — start with `internal/app/app.go`.

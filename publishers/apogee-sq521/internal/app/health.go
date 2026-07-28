package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdnotify"
)

// runHealth pets systemd's watchdog for as long as the poll goroutine is
// scheduling, and stops the moment it is not.
//
// # What the heartbeat means
//
// "The poll goroutine is running and making progress" — never "the sensor is
// answering". The distinction is the whole design. A sensor that has been
// unplugged must produce a retained "offline" and a steady retry, not a systemd
// kill loop: restarting the process re-enumerates the USB device every
// WatchdogSec, which thrashes the bus and loses the retained state that told an
// operator what was wrong. So the poll goroutine stamps the heartbeat on every
// iteration regardless of what the sensor said, and even while it is backing
// off waiting for a device that does not exist.
//
// What the watchdog therefore catches is the failure it is actually good for:
// the goroutine itself stopped. A wedged serial read with no deadline, a
// deadlocked publish, a panic in a helper — anything that parks the loop stops
// the stamp, this loop stops pinging, and systemd's WatchdogSignal (SIGABRT by
// default) dumps every goroutine's stack into the journal before the restart.
// That dump is worth more than any log line we could write about a hang we do
// not understand.
//
// Outside systemd both notifications are no-ops, so the same binary runs
// unchanged under a bare shell. The loop still runs: the status line is useful
// wherever NOTIFY_SOCKET is set, and a unit that configures Type=notify without
// WatchdogSec used to get no STATUS= at all — leaving `systemctl status` blank
// and "healthy", "wedged" and "broker partitioned" indistinguishable from each
// other on a binary that logs nothing on the happy path.
func (a *App) runHealth(ctx context.Context) {
	interval, armed := a.deps.Notifier.WatchdogInterval()

	// Half the interval is the conventional margin: it survives one lost
	// datagram, and sdnotify's writes are bounded at 100 ms so a slow manager
	// cannot eat the rest.
	tick := statusRefresh
	if armed {
		tick = interval / 2
		if tick <= 0 {
			tick = interval
		}
	}
	tolerance := a.stalenessTolerance()
	if armed {
		a.log.Info("watchdog armed", "interval", interval, "ping_every", tick, "tolerance", tolerance)
	} else {
		// Info, not Debug: the process logger is pinned to LevelInfo, so a Debug
		// line here says nothing at all — and "no watchdog" is exactly what an
		// operator needs to know before concluding from a silent journal that
		// the daemon is fine.
		a.log.Info("no service-manager watchdog configured; refreshing the status line only",
			"status_every", tick,
			"note", "set WatchdogSec= in the unit to have systemd restart a wedged poll loop")
	}

	var hs healthState
	for {
		if !a.wait(ctx, tick) {
			return
		}

		if armed {
			since := a.clock.Now().Sub(a.lastBeat())
			if since >= tolerance {
				if !hs.stale {
					hs.stale = true
					a.log.Error("poll goroutine has not made progress; withholding the watchdog ping so the service manager can restart us and dump the stacks",
						"since_last_heartbeat", since, "tolerance", tolerance)
				}
				// Deliberately keep looping rather than returning. systemd is
				// about to kill us either way, but if WatchdogSec is generous
				// enough for the loop to un-wedge on its own, resuming the ping
				// is strictly better than having thrown the ability away. The
				// status line keeps going too — it is the one thing that can
				// still say what happened.
				a.status(&hs)
				continue
			}
			if hs.stale {
				hs.stale = false
				a.log.Info("poll goroutine is making progress again; resuming watchdog pings",
					"since_last_heartbeat", since)
			}
			a.ping(&hs)
		}
		a.status(&hs)
	}
}

// statusRefresh is how often the one-line status is rebuilt when there is no
// watchdog interval to derive a cadence from. It is a fixed constant rather
// than a function of POLL_SECONDS because a 1 s poll interval would otherwise
// produce a datagram a second for a line nobody is reading.
const statusRefresh = 30 * time.Second

// healthState is the health goroutine's own bookkeeping. It lives on that
// goroutine's stack and is shared with nothing.
type healthState struct {
	// stale records that the ping is currently being withheld, so the decision
	// is logged when it is taken and when it is reversed.
	stale bool
	// pingFailing is the same transition guard for the notification socket. A
	// dial failure repeats every tick — thousands a day at WatchdogSec=30 — and
	// it is the one recurring line the rest of this daemon guards against
	// everywhere else.
	pingFailing bool
	// statusFailing guards the status notification the same way.
	statusFailing bool
}

// stalenessTolerance is how long the poll goroutine may go without stamping
// before it counts as wedged.
//
// It is a sum of the spans the goroutine can legitimately spend not stamping,
// because they follow one another rather than overlapping: a dial that fails
// parks on the backoff, a session that opens probes and births, a cycle spends
// its budget, and the next boundary is a poll interval away. Every individual
// operation inside those spans stamps — probe beats between transactions,
// publish beats after each message — so this is a ceiling on the *arrangement*,
// not on any one wait.
//
//   - dialBackoffMax: the longest park between two open attempts, and the only
//     wait in the daemon longer than a poll interval.
//   - 2 × PollInterval: one boundary wait plus the scheduling jitter of a Pi
//     that is also running something else.
//   - cycleBudget: what a single cycle may legitimately spend on a slow sensor.
//   - birthBudget: a reconnection births on autopaho's goroutine while holding
//     seqMu, and the poll goroutine waits for it without stamping.
//
// Anything tighter turns a slow sensor or a slow broker into a restart, and a
// restart re-enumerates the USB adapter — which is the failure mode a watchdog
// is supposed to prevent rather than cause. The measured worst case before
// these terms were added was 79 s against a 53 s tolerance, reached from the
// ordinary dead-sensor path.
func (a *App) stalenessTolerance() time.Duration {
	return dialBackoffMax + 2*a.cfg.PollInterval + a.cycleBudget + a.birthBudget
}

// lastBeat reads the heartbeat the poll goroutine stamps.
func (a *App) lastBeat() time.Time {
	return time.Unix(0, a.heartbeat.Load())
}

// ping sends WATCHDOG=1.
//
// A missed notification is logged at debug and never treated as a fault:
// sdnotify bounds the write at 100 ms precisely so a service manager that is
// not draining cannot park this goroutine, and a manager that is not draining
// is not a reason to consider the daemon unhealthy. Anything else is logged on
// transition, because a socket that cannot be dialled cannot be dialled every
// tick either.
func (a *App) ping(hs *healthState) {
	err := a.deps.Notifier.Watchdog()
	switch {
	case err == nil:
		if hs.pingFailing {
			hs.pingFailing = false
			a.log.Info("watchdog ping recovered")
		}
	case errors.Is(err, sdnotify.ErrNotifyTimeout):
		a.log.Debug("watchdog ping timed out", "error", err)
	case !hs.pingFailing:
		hs.pingFailing = true
		a.log.Warn("watchdog ping failed", "error", err,
			"note", "further failures are logged at debug until one succeeds")
	default:
		a.log.Debug("watchdog ping still failing", "error", err)
	}
}

// status refreshes the one line `systemctl status` shows.
func (a *App) status(hs *healthState) {
	err := a.deps.Notifier.Status(a.statusLine())
	switch {
	case err == nil:
		hs.statusFailing = false
	case errors.Is(err, sdnotify.ErrNotifyTimeout):
		a.log.Debug("status notification timed out", "error", err)
	case !hs.statusFailing:
		hs.statusFailing = true
		a.log.Debug("status notification failed", "error", err)
	}
}

// statusLine is the one line `systemctl status` shows.
//
// It answers the questions an operator actually has at a terminal — is there
// light, how old is that number, is the sensor answering, is the bus up, is
// anything missing — without needing the journal.
//
// THE TRAP this closes: it used to derive everything from App.online and
// App.lastValues, both of which are written only *after a successful publish*.
// When the broker link dropped, nothing moved: the sensor was fine so the
// ladder never fired, so nothing republished the availability, so the line kept
// saying "mqtt up" and kept printing an hours-old reading as current. The
// operator SSHes in because readings stopped appearing in grow-app, and the one
// line they read affirmatively clears the component that is broken. So the
// broker gets its own token, sourced from the publish path — the only thing in
// the daemon that observes the link — the sensor gets a separate one from the
// failure ladder, and the reading carries its age.
func (a *App) statusLine() string {
	now := a.clock.Now()

	a.mu.Lock()
	ppfd, hasPPFD := a.lastValues[ppfdObjectID]
	dliNow, hasDLI := a.lastValues["dli"]
	readingAt := a.lastReadingAt
	degraded := a.degraded
	linkFailing, linkSeen, lastOK := a.publishFailing, a.publishSeen, a.lastPublishAt
	table := a.table
	unsupported := a.snapshot.Unsupported
	a.mu.Unlock()

	parts := make([]string, 0, 6)
	switch {
	case hasPPFD && ppfd != "" && !readingAt.IsZero():
		parts = append(parts, "PPFD "+ppfd+" "+a.cfg.PPFDUnit+" ("+since(now, readingAt)+" old)")
	default:
		parts = append(parts, "PPFD n/a")
	}
	if hasDLI && dliNow != "" {
		parts = append(parts, "DLI "+dliNow+" mol")
	}

	if degraded {
		parts = append(parts, "sensor not answering")
	} else {
		parts = append(parts, "sensor ok")
	}

	switch {
	case !linkSeen:
		parts = append(parts, "mqtt connecting")
	case !linkFailing:
		parts = append(parts, "mqtt up")
	case lastOK.IsZero():
		parts = append(parts, "mqtt DOWN (never connected)")
	default:
		parts = append(parts, "mqtt DOWN (last publish "+since(now, lastOK)+" ago)")
	}

	for _, e := range table {
		if e.Kind == entities.SourcePolled && e.Optional && unsupported[e.ObjectID] {
			parts = append(parts, e.ObjectID+" n/a")
		}
	}
	return strings.Join(parts, " · ")
}

// since renders an age for the status line, rounded to the second because
// nothing an operator does with it needs more.
func since(now, then time.Time) string {
	d := now.Sub(then)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// runTimezone applies reconciler writes and echoes what was accepted.
//
// It is a goroutine of its own because mqttpub delivers an inbound PUBLISH on
// the network goroutine that received it and documents that the callback must
// not block — while applying a zone writes a file on an SD card and echoing it
// is an MQTT publish with a two-second budget. Doing either inline would stall
// autopaho's reader, and with it the keepalive that decides how quickly the
// will fires.
func (a *App) runTimezone(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case value := <-a.tzCmd:
			a.applyTimezone(ctx, value)
		}
	}
}

// applyTimezone runs one reconciler write end to end.
//
// The echo is unconditional and is the exact bytes timezone.Manager returns:
// grow-app decides push-versus-in-sync by comparing our retained state against
// the value it wants, so a rejected write has to re-echo the zone still in
// force. Echoing the received bytes instead would tell the reconciler its value
// took when it did not, and it would stop pushing.
func (a *App) applyTimezone(ctx context.Context, value string) {
	echo := a.deps.Zones.Apply(value)
	// changeOnly: the reconciler re-pushes on its own schedule and on every
	// reconnect, and the overwhelmingly common case is a push of the value
	// already in force.
	//
	// In a sequence like every other publish path, so the echo cannot land
	// between a birth's discovery configs and the availability that follows
	// them.
	a.sequence(func() { a.publishState(ctx, timeZoneComponent, timeZoneObjectID, echo, true) })

	// dli decides local midnight from this. Applied after the echo because the
	// echo is what unblocks the reconciler, and re-filing a day is the slower
	// of the two.
	a.deps.Integrator.SetLocation(a.deps.Zones.Location(), a.deps.Zones.State())
}

// onTimezoneCommand is the mqttpub subscription callback. It runs on autopaho's
// network goroutine, so it does nothing but hand the value across.
//
// A full channel drops the incoming value rather than blocking. That is safe
// because the reconciler is level-triggered: it compares our retained state
// against what it wants and pushes again until they match, so a dropped push
// costs one round of its schedule and nothing else. Blocking here would cost
// the MQTT keepalive.
func (a *App) onTimezoneCommand(topic string, payload []byte) {
	select {
	case a.tzCmd <- string(payload):
	default:
		a.log.Debug("dropped a timezone push while the previous one was still being applied; the reconciler re-pushes",
			"topic", topic)
	}
}

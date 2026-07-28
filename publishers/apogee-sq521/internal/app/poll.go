package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

// ppfdObjectID is the reading the whole device exists for, and the one fed to
// the DLI accumulator.
const ppfdObjectID = "ppfd"

// calFactorObjectID is the static whose payload comes from the first 0V!
// coefficient.
const calFactorObjectID = "cal_factor"

// subGroup is one aM<sub>! transaction and every entity its data response
// feeds.
//
// Grouping by sub-command rather than by entity is not an optimisation, it is
// the protocol: 0M! returns whatever the sensor has for that measurement, and
// if its header reports n >= 2 then PPFD and detector millivolts arrive in the
// same data response and asking twice would be two conversions for one answer.
//
// The table is deliberately NOT pre-collapsed on that speculation. The Python
// parser took findall(...)[0] and discarded the rest, so the evidence for what
// 0M! actually reports is not in the code — which is why the observed value
// count is logged once per port session, so the table can be retuned from the
// journal rather than from a guess.
type subGroup struct {
	sub      string
	entities []entities.Entity
}

// groupBySubCmd collapses the polled entities of a table into one group per
// distinct sub-command, preserving table order so the publish order is stable.
func groupBySubCmd(table []entities.Entity) []subGroup {
	var groups []subGroup
	at := map[string]int{}
	for _, e := range table {
		if e.Kind != entities.SourcePolled {
			continue
		}
		i, ok := at[e.SubCmd]
		if !ok {
			i = len(groups)
			at[e.SubCmd] = i
			groups = append(groups, subGroup{sub: e.SubCmd})
		}
		groups[i].entities = append(groups[i].entities, e)
	}
	return groups
}

// pollState is the poll goroutine's own bookkeeping. It lives on that
// goroutine's stack and is shared with nothing, which is why none of it is
// locked — everything another goroutine can see lives on App under App.mu.
type pollState struct {
	// failures counts consecutive failed attempts to obtain a reading, by any
	// cause — a failed dial counts as much as a failed cycle — and drives both
	// halves of the availability ladder: the state topics blank on the first,
	// and the retained "offline" lands on cfg.OfflineAfter. It is a count of
	// attempts and NOT a measure of elapsed time, which is why failingSince
	// exists.
	failures int
	// failingSince is when the current run of failures started, so the recovery
	// line can report the outage in the units an operator thinks in.
	failingSince time.Time
	// dials counts consecutive failed opens, which is a different thing from a
	// failed cycle: one is "there is no device", the other is "the device is not
	// answering", and they are retried on different schedules.
	dials int
	// dialCeiling records that the backoff has reached dialBackoffMax and been
	// reported, so the journal states the retry gap that is actually in force.
	dialCeiling bool
	// sessions counts port sessions opened since the last successful reading, so
	// the recovery line can say how much churn the outage cost.
	sessions int
	// soft counts consecutive failures blamed on a malformed frame or silence
	// — neither of which implicates the descriptor — and escalates to a reopen
	// once the adapter has clearly stopped being merely unlucky.
	soft int
	// reopenLogged suppresses the reopen warning after the first one of a
	// failure run. A mute-but-enumerated sensor reopens every
	// reopenAfterSoftFailures cycles forever, and an unguarded line here buries
	// the one that says when the outage started.
	reopenLogged bool
	// overrunning records whether the previous cycle missed its boundary, so an
	// overrun is logged when it starts and when it stops rather than every
	// cycle.
	overrunning bool
	// budgetWarned suppresses the "raise maxConversionWait" advice after the
	// first time; it is a build-time constant, so repeating it says nothing new.
	budgetWarned bool
	// missedReported is the per-session missed-service-request count last
	// written to the journal, so a steady rate reports once rather than once per
	// session.
	missedReported uint64
	// probeReported records, per probe step, the answer already reported. The
	// probe runs once per port session and every reopen is a fresh session, so
	// without this a sensor that will not identify itself writes three warnings
	// per session for as long as it stays that way — thousands a day, burying
	// the onset.
	probeReported map[string]string
	// seen records the value count observed per sub-command in this port
	// session. Reset on every reopen, because a re-enumerated adapter is worth
	// re-observing.
	seen map[string]int
}

// report says whether this step's answer differs from the one already written
// to the journal, and records it. It is the transition guard every recurring
// probe line goes through.
func (st *pollState) report(step, outcome string) bool {
	if st.probeReported == nil {
		st.probeReported = map[string]string{}
	}
	if prev, ok := st.probeReported[step]; ok && prev == outcome {
		return false
	}
	st.probeReported[step] = outcome
	return true
}

// cycleOutcome says what the poll loop should do next.
type cycleOutcome int

const (
	// outcomeContinue schedules the next boundary, whether or not the sensor
	// answered.
	outcomeContinue cycleOutcome = iota
	// outcomeReopen abandons the port session: close, settle, re-probe.
	outcomeReopen
	// outcomeShutdown means the root context ended.
	outcomeShutdown
)

// runPoller owns the serial port for the life of the daemon.
//
// The open lives here rather than in Run because MQTT must be up first: the
// Python retried the open for up to 36 s before configuring MQTT, so no will
// was registered during exactly the window where the daemon was most likely to
// die. Here an adapter that never enumerates produces a retained "offline" and
// a steady retry, which is a visible outage rather than an invisible one.
func (a *App) runPoller(ctx context.Context) {
	// Marked once, here, so every publish reached from this goroutine — and only
	// from this goroutine — stamps the heartbeat. See pollerCtxKey.
	ctx = withPoller(ctx)

	st := &pollState{}
	backoff := dialBackoffMin

	for ctx.Err() == nil {
		// Stamped before the dial as well as inside the cycle. The heartbeat
		// means "the poll goroutine is scheduling and making progress", not
		// "the sensor is answering" — a watchdog that killed us for an absent
		// adapter would thrash the USB bus on a restart loop instead of leaving
		// the retained "offline" standing and retrying.
		a.beat()

		s, err := a.deps.Dialer.Dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			st.dials++
			a.sequence(func() { a.recordFailure(ctx, st, err, "serial open failed") })
			a.noteDialFailure(st, backoff, err)
			if !a.wait(ctx, backoff) {
				return
			}
			backoff = min(2*backoff, dialBackoffMax)
			continue
		}
		backoff = dialBackoffMin
		st.dials, st.dialCeiling = 0, false
		st.sessions++
		// Reported at Info only while nothing is wrong. A mute sensor churns a
		// session every few cycles for as long as it stays mute, and the count
		// that matters is the one the recovery line carries.
		if st.failures == 0 {
			a.log.Info("serial session open", "device", s.String())
		} else {
			a.log.Debug("serial session reopened while the sensor is not answering",
				"device", s.String(), "sessions_this_outage", st.sessions)
		}

		a.runSession(ctx, s, st)

		if n := s.MissedServiceRequests(); n != st.missedReported {
			st.missedReported = n
			if n > 0 {
				a.log.Info("adapter swallowed data-ready notifications during this session",
					"count", n,
					"note", "a count that rises across sessions means the USB bridge, not the sensor, is dropping traffic")
			}
		}
		if err := s.Close(); err != nil {
			a.log.Warn("closing the serial port", "device", s.String(), "error", err)
		}
	}
}

// noteDialFailure reports the retry schedule that is actually in force.
//
// The gap doubles from dialBackoffMin to dialBackoffMax, so a single line at
// the first failure states a number that is wrong for every retry after it: an
// operator who re-seats the adapter reads "retry_in=1s", sees nothing for
// thirty seconds and restarts the daemon believing it wedged. Two lines — the
// first failure and the arrival at the ceiling — bracket the whole schedule
// without repeating themselves.
func (a *App) noteDialFailure(st *pollState, backoff time.Duration, err error) {
	switch {
	case st.dials == 1:
		a.log.Warn("could not open the serial device; retrying",
			"device", a.cfg.SDI12Port, "retry_in", backoff,
			"note", "the gap doubles up to "+dialBackoffMax.String(), "error", err)
	case backoff >= dialBackoffMax && !st.dialCeiling:
		st.dialCeiling = true
		a.log.Warn("the serial device is still absent; retrying at the backoff ceiling",
			"device", a.cfg.SDI12Port, "retry_in", dialBackoffMax,
			"failed_opens", st.dials, "error", err)
	}
}

// runSession probes one port session and then cycles until the port has to be
// reopened or the daemon is stopping.
func (a *App) runSession(ctx context.Context, s Session, st *pollState) {
	st.seen = map[string]int{}
	st.soft = 0

	res := a.probe(ctx, s, st)
	if ctx.Err() != nil {
		return
	}
	a.adopt(res)

	// Only now. Publishing discovery before this point announces tilt on a
	// pre-3033 unit and then retires it on the first poll, leaving a retained
	// config whose state topic never fills — the Python's on_connect defect,
	// exactly.
	a.publishBirth(ctx, birthLocal)

	next := a.clock.Now()
	for {
		switch a.cycle(ctx, s, st) {
		case outcomeShutdown, outcomeReopen:
			return
		}

		now := a.clock.Now()
		boundary, skipped := nextBoundary(next, a.cfg.PollInterval, now)
		a.noteOverrun(st, skipped, now.Sub(next))
		next = boundary

		if !a.wait(ctx, next.Sub(now)) {
			return
		}
	}
}

// nextBoundary returns the first instant at or after now that is a whole number
// of intervals past prev, and how many boundaries were missed getting there.
//
// This is what makes the cadence absolute. The Python slept a flat POLL_SECONDS
// *after* its three serial transactions, so a 10 s setting produced a real
// 13-16 s cadence — every conversion, settle and retry added itself to the
// period permanently. Sleeping interval-minus-elapsed instead keeps the samples
// on a grid, which matters downstream: the DLI integrator's max-gap guard is
// calibrated against the sample spacing, and a drifting one would look like
// jitter it has to absorb.
//
// An overrun skips whole boundaries rather than catching up with back-to-back
// cycles, because a cycle that took longer than the interval is a sensor that
// is already struggling and firing three at once would only make it worse.
func nextBoundary(prev time.Time, interval time.Duration, now time.Time) (time.Time, int) {
	next := prev.Add(interval)
	if !next.Before(now) {
		return next, 0
	}
	// Whole intervals to add so that next lands at or after now. The ceiling —
	// not the floor — because landing exactly on now is on the grid and firing
	// immediately there is right, while landing before it is not a boundary at
	// all.
	behind := now.Sub(next)
	skipped := int(behind / interval)
	if behind%interval != 0 {
		skipped++
	}
	return next.Add(time.Duration(skipped) * interval), skipped
}

// noteOverrun logs an overrun when it starts and when it stops, never in
// between. A cycle that keeps missing its boundary would otherwise emit a line
// every interval forever — the same defect as the Python's "no reading for
// tilt" every 10 s, which was as much a logging fault as a protocol one.
func (a *App) noteOverrun(st *pollState, skipped int, elapsed time.Duration) {
	switch {
	case skipped > 0 && !st.overrunning:
		st.overrunning = true
		a.log.Warn("poll cycle overran its boundary; skipping ahead rather than drifting",
			"skipped_boundaries", skipped, "cycle_elapsed", elapsed, "interval", a.cfg.PollInterval)
	case skipped == 0 && st.overrunning:
		st.overrunning = false
		a.log.Info("poll cycle is back on its boundary", "interval", a.cfg.PollInterval)
	}
}

// serialObjectID is the static whose payload comes from the 0I! response.
const serialObjectID = "sensor_serial"

// probeResult is one port session's probe.
type probeResult struct {
	// snap is the Snapshot the session produced on its own, before anything the
	// previous session knew is carried forward.
	snap entities.Snapshot

	// inconclusive lists the static object ids whose probe step *errored*, as
	// opposed to answering that the sensor has no such value.
	//
	// THE TRAP this closes: a static missing from Statics is ineligible, and an
	// ineligible entity is retracted — so a single 0I! or 0V! timeout would
	// delete two established retained configs and their state topics. The timing
	// makes it worse than it sounds: the reopen that re-probes fires at
	// reopenAfterSoftFailures, which is exactly when the sensor is flaky and a
	// probe step is most likely to time out, and the entities then stay
	// retracted until some later reopen whose probe happens to succeed.
	//
	// probeCapabilities has always been scrupulous about this — it keys strictly
	// on ErrHeaderNoValues and keeps the entity on anything else. This is the
	// same principle applied to the path that actually deletes: only a
	// definitive negative retracts, and an inconclusive answer carries the
	// previous session's value forward instead. See App.adopt.
	inconclusive map[string]bool

	// identityUnknown means 0I! failed, so an empty SWVersion says nothing about
	// the firmware and the previous session's is still the best answer. Without
	// it every discovery config's device block changes on a flaky reopen, which
	// republishes the whole set for no new information.
	identityUnknown bool
}

// probe runs once per port session and produces the Snapshot everything else is
// derived from: the firmware for the device block, the two static payloads, and
// the definitive capability answers.
//
// Nothing here is fatal. A sensor that will not identify itself still measures,
// and entities.Eligible simply leaves the statics unannounced rather than
// announcing entities whose state topic would never fill.
//
// It beats between transactions. A probe is up to three serial transactions and
// on a silent sensor each one spends its whole budget, which is far longer than
// a poll cycle — and the poll goroutine is demonstrably scheduling throughout.
// An un-beaten probe is reached from the ordinary sensor-failure path, not a
// contrived one, and the restart it used to provoke re-enumerates the USB
// adapter: precisely the thrash runHealth exists to avoid.
func (a *App) probe(ctx context.Context, s Session, st *pollState) probeResult {
	res := probeResult{
		snap: entities.Snapshot{
			Statics:     map[string]string{},
			Unsupported: map[string]bool{},
		},
		inconclusive: map[string]bool{},
	}
	budget := transactionBudget(a.cfg.ReadTimeout)

	a.beat()
	ictx, cancel := context.WithTimeout(ctx, budget)
	id, err := s.Identify(ictx)
	cancel()
	a.beat()
	switch {
	case err != nil:
		res.identityUnknown = true
		res.inconclusive[serialObjectID] = true
		// Not on the way out: cancelling the root context shortens the port's
		// deadline (see serialSession.watch), which surfaces here as a read
		// timeout. Reporting it would put a sensor fault in the journal for
		// every clean `systemctl stop`.
		if ctx.Err() == nil && st.report("identify", "failed") {
			a.log.Warn("sensor identification failed; keeping whatever a previous session read",
				"command", a.cfg.SDI12Address+"I!", "error", err)
		}
	default:
		res.snap.SWVersion = id.Firmware
		if id.Serial != "" {
			res.snap.Statics[serialObjectID] = id.Serial
		}
		if st.report("identify", "ok:"+id.Firmware+"/"+id.Serial) {
			a.log.Info("sensor identified",
				"vendor", id.Vendor, "model", id.Model,
				"firmware", id.Firmware, "serial", id.Serial, "sdi12_version", id.Version)
		}
	}

	vctx, cancel := context.WithTimeout(ctx, budget)
	cal, err := s.Verify(vctx)
	cancel()
	a.beat()
	switch {
	case err != nil:
		res.inconclusive[calFactorObjectID] = true
		if ctx.Err() == nil && st.report("verify", "failed") {
			a.log.Warn("calibration read (0V!) failed; keeping whatever a previous session read",
				"command", a.cfg.SDI12Address+"V!", "error", err)
		}
	case len(cal) == 0:
		// Answered, and the answer is "there is no coefficient". Definitive, so
		// cal_factor is genuinely ineligible and its config is retracted.
		if st.report("verify", "empty") {
			a.log.Warn("calibration read (0V!) returned no coefficients; cal_factor will not be announced")
		}
	default:
		// Formatted through the table's own row so the %.6g the Python used is
		// stated in exactly one place. entities.Format panics on an unset
		// FormatKind by design; the lookup is what keeps that unreachable.
		if e, ok := findEntity(a.table, calFactorObjectID); ok {
			res.snap.Statics[calFactorObjectID] = entities.Format(e, cal[0])
			if st.report("verify", "ok:"+res.snap.Statics[calFactorObjectID]) {
				a.log.Info("calibration multiplier read",
					"cal_factor", res.snap.Statics[calFactorObjectID])
			}
		}
	}

	a.probeCapabilities(ctx, s, &res.snap, budget, st)
	return res
}

// probeCapabilities asks once, per port session, about every optional
// measurement.
//
// The gate is the measurement HEADER, never a missing data line.
// sdi12.ErrNoValues covers both — a header declaring zero values *and* a header
// that promised n and was followed by a bare aD0! reply, which is an adapter
// swallowing traffic. Retiring an entity on the second would take a working
// channel off the bus for the life of the process after one glitch, so only
// sdi12.ErrHeaderNoValues counts.
func (a *App) probeCapabilities(ctx context.Context, s Session, snap *entities.Snapshot, budget time.Duration, st *pollState) {
	for _, g := range groupBySubCmd(a.table) {
		optional := false
		for _, e := range g.entities {
			optional = optional || e.Optional
		}
		if !optional {
			continue
		}

		pctx, cancel := context.WithTimeout(ctx, budget)
		vals, err := s.Measure(pctx, g.sub)
		cancel()
		a.beat()

		step := "capability:" + g.sub
		switch {
		case err == nil:
			if st.report(step, "supported") {
				a.log.Info("optional measurement supported",
					"command", a.measureCommand(g.sub), "values", len(vals))
			}
		case errors.Is(err, sdi12.ErrHeaderNoValues):
			for _, e := range g.entities {
				if e.Optional {
					snap.Unsupported[e.ObjectID] = true
				}
			}
			if st.report(step, "unsupported") {
				a.log.Info("optional measurement not implemented by this unit; retiring its entities",
					"command", a.measureCommand(g.sub), "entities", optionalIDs(g), "error", err)
			}
		case ctx.Err() != nil:
			return
		default:
			// Inconclusive. Keeping the entity is the safe direction: a
			// transient fault costs a blank state topic until the next cycle,
			// while retiring on it costs the entity until the next restart.
			if st.report(step, "inconclusive") {
				a.log.Warn("optional measurement probe inconclusive; keeping its entities",
					"command", a.measureCommand(g.sub), "entities", optionalIDs(g), "error", err)
			}
		}
	}
}

// reading is one entity's outcome for one cycle.
type reading struct {
	entity entities.Entity
	value  float64
	ok     bool
}

// measureResult summarises a cycle's transactions.
type measureResult struct {
	// ok counts transactions that produced at least one value.
	ok int
	// fails counts transactions that returned an error, and transient counts
	// the subset attributable to a malformed frame or silence.
	fails     int
	transient int
	// portDead stops the cycle immediately: further commands on a dead
	// descriptor can only spin.
	portDead bool
	// retired records that an optional entity was definitively retired, which
	// obliges the caller to retract its retained discovery config.
	retired bool
	// first is the error to name in the transition log.
	first error
}

// cycle runs one measurement pass and publishes its consequences.
func (a *App) cycle(ctx context.Context, s Session, st *pollState) cycleOutcome {
	a.beat()

	// Sized from the sensor's conversion time, not from POLL_SECONDS. See
	// maxConversionWait: a budget below ttt + 1 s does not merely truncate the
	// wait, it makes sdi12 refuse the aD0! and every cycle fail.
	cctx, cancel := context.WithTimeout(ctx, a.cycleBudget)
	defer cancel()

	readings, res := a.measure(cctx, s, groupBySubCmd(a.workingSet()), st)

	if ctx.Err() != nil {
		return outcomeShutdown
	}

	success := res.ok > 0

	// One sequence, so a reconnection birth on autopaho's goroutine cannot land
	// a discovery config between this cycle's availability and its values.
	a.sequence(func() {
		// Retraction first, so a retired entity's blank state is not republished
		// to a topic whose config is about to be cleared.
		if res.retired {
			a.birth(ctx, birthLocal)
			readings = a.filterEligible(readings)
		}

		if success && st.failures > 0 {
			// Recovery. Discovery and the birth "online" go out before the fresh
			// value, so a subscriber that sees the reading has already seen the
			// entity it belongs to.
			//
			// The outage is reported as a duration as well as an attempt count:
			// failures counts attempts of two different kinds on two different
			// schedules, so on its own it says nothing about how long the sensor
			// was out.
			a.log.Info("sensor readings recovered",
				"failed_attempts", st.failures,
				"outage", a.clock.Now().Sub(st.failingSince).Round(time.Second),
				"port_sessions", st.sessions)
			st.failures, st.soft = 0, 0
			st.sessions, st.reopenLogged = 0, false
			a.setDegraded(false)
			a.birth(ctx, birthLocal)
		}

		a.publishReadings(ctx, readings)
		a.integrate(ctx, readings)

		if !success {
			a.recordFailure(ctx, st, res.first, "no measurement produced a value")
		}
	})

	if !success {
		if res.transient == res.fails && res.fails > 0 {
			st.soft++
		} else {
			st.soft = 0
		}
	}

	switch {
	case res.portDead:
		a.log.Warn("serial port is unusable; reopening", "error", res.first)
		return outcomeReopen
	case st.soft >= reopenAfterSoftFailures:
		// Once per outage, not once per reopen. A sensor answering nothing
		// usable reopens every reopenAfterSoftFailures cycles for as long as it
		// stays that way, and at a 10 s cadence an unguarded line here is
		// thousands a day — enough that `journalctl -n 200` returns nothing but
		// this and never reaches the line that says when it started.
		if !st.reopenLogged {
			st.reopenLogged = true
			a.log.Warn("consecutive transient sensor failures; reopening the port",
				"cycles", st.soft, "error", res.first,
				"note", "further reopens during this outage are logged at debug")
		} else {
			a.log.Debug("reopening the port again", "cycles", st.soft, "error", res.first)
		}
		return outcomeReopen
	}
	return outcomeContinue
}

// measure runs one transaction per distinct sub-command and routes the values
// back onto the entities bound to them.
func (a *App) measure(ctx context.Context, s Session, groups []subGroup, st *pollState) ([]reading, measureResult) {
	var out []reading
	var res measureResult

	for _, g := range groups {
		vals, err := s.Measure(ctx, g.sub)
		if err != nil {
			res.fails++
			if res.first == nil {
				res.first = err
			}
			a.classify(err, g, st, &res)
			for _, e := range g.entities {
				out = append(out, reading{entity: e})
			}
			if res.portDead {
				return out, res
			}
			continue
		}

		res.ok++
		a.noteValueCount(st, g.sub, len(vals))
		for _, e := range g.entities {
			if e.Index < len(vals) {
				out = append(out, reading{entity: e, value: vals[e.Index], ok: true})
				continue
			}
			// The header promised fewer values than the table expects. Not a
			// fault of this transaction — the values that did arrive are good —
			// so the entity simply has no reading and its state blanks.
			out = append(out, reading{entity: e})
		}
	}
	return out, res
}

// classify routes one transaction failure onto the recovery this package owns.
// The order mirrors sdi12's documented classification order.
func (a *App) classify(err error, g subGroup, st *pollState, res *measureResult) {
	switch {
	case errors.Is(err, context.Canceled):
		// Shutting down. Not a statement about the sensor.

	case errors.Is(err, sdi12.ErrPortDead):
		res.portDead = true

	case errors.Is(err, sdi12.ErrHeaderNoValues):
		// The one definitive capability answer. Only optional entities may be
		// retired by it; entities.Eligible ignores Unsupported for the rest, and
		// setting it for them anyway would be a lie in the Snapshot.
		for _, e := range g.entities {
			if e.Optional && a.retire(e.ObjectID) {
				res.retired = true
				a.log.Info("measurement retired mid-run; its retained config will be cleared",
					"command", a.measureCommand(g.sub), "object_id", e.ObjectID, "error", err)
			}
		}

	case errors.Is(err, context.DeadlineExceeded):
		// Our own budget, not the sensor's silence: sdi12 refuses to send aD0!
		// when the context truncated the conversion wait, which is the correct
		// behaviour and a sign this daemon's ceiling is too low.
		if !st.budgetWarned {
			st.budgetWarned = true
			a.log.Error("the per-cycle budget is shorter than the sensor's declared conversion time; raise maxConversionWait",
				"command", a.measureCommand(g.sub), "cycle_budget", a.cycleBudget,
				"max_conversion_wait", maxConversionWait, "error", err)
		}

	case errors.Is(err, sdi12.ErrProtocol), errors.Is(err, sdi12.ErrNoResponse):
		// sdi12 has already drained the port for ErrProtocol, so both are
		// "retry next cycle" — but a run of them escalates to a reopen.
		res.transient++

	default:
		// Includes sdi12.ErrNoValues that is not header-declared: the header
		// promised values and the data response arrived bare. Transient by
		// definition, and explicitly NOT grounds to retire anything.
		res.transient++
	}
}

// retire records an optional entity as unsupported and recomputes the working
// set. It reports whether this changed anything.
//
// The Unsupported map is replaced rather than mutated: copies of the Snapshot
// are handed to the birth sequence, which may be running on another goroutine,
// and a map shared between them would be a data race that no amount of locking
// at this end could fix.
func (a *App) retire(objectID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.snapshot.Unsupported[objectID] {
		return false
	}
	next := make(map[string]bool, len(a.snapshot.Unsupported)+1)
	for k, v := range a.snapshot.Unsupported {
		next[k] = v
	}
	next[objectID] = true
	a.snapshot.Unsupported = next
	a.working = entities.EligibleSet(a.table, a.snapshot)
	return true
}

// adopt installs a fresh Snapshot and resolves the working set from it — the
// single funnel both discovery and polling read.
//
// What an inconclusive probe step could not re-read is carried forward from the
// Snapshot it replaces, so a transient failure never demotes an entity that was
// previously established. Only a definitive negative can: the sensor answering
// 0V! with no coefficients, or a header declaring zero values. See
// probeResult.inconclusive for why the distinction is load-bearing.
//
// The carry-forward is deliberately one-directional. It cannot resurrect an
// entity the sensor has said it does not have, because Unsupported is taken
// from the fresh Snapshot alone: a capability probe that errors leaves the
// entity out of Unsupported anyway, which already means "keep it".
//
// The carry-forward alone is not the whole rule, because it is a statement about
// the Snapshot being *replaced* and a fresh process has none. The inconclusive
// set is therefore published to App as well, where retract consults it
// regardless of a.probed: the first probe of a new process is exactly as
// entitled to fail transiently as the tenth of an old one, and a restart is the
// commonest way to arrive at one. See App.retract.
func (a *App) adopt(p probeResult) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Safe to hold rather than clone: probe builds it fresh per call and nothing
	// writes to it afterwards.
	a.inconclusive = p.inconclusive

	snap := p.snap
	if a.probed {
		if p.identityUnknown && snap.SWVersion == "" {
			snap.SWVersion = a.snapshot.SWVersion
		}
		for objectID := range p.inconclusive {
			if _, fresh := snap.Statics[objectID]; fresh {
				continue
			}
			if prev, had := a.snapshot.Statics[objectID]; had {
				snap.Statics[objectID] = prev
			}
		}
	}

	a.snapshot = snap
	a.working = entities.EligibleSet(a.table, snap)
	a.probed = true
}

// workingSet returns a copy of the entities currently eligible.
func (a *App) workingSet() []entities.Entity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.working)
}

// filterEligible drops readings for entities that are no longer in the working
// set, which is what a mid-run retirement leaves behind.
func (a *App) filterEligible(readings []reading) []reading {
	live := map[string]bool{}
	for _, e := range a.workingSet() {
		live[e.ObjectID] = true
	}
	out := readings[:0:0]
	for _, r := range readings {
		if live[r.entity.ObjectID] {
			out = append(out, r)
		}
	}
	return out
}

// integrate feeds PPFD into the DLI accumulator and publishes whatever that
// produced, including a midnight rollover.
func (a *App) integrate(ctx context.Context, readings []reading) {
	for _, r := range readings {
		if !r.ok || r.entity.ObjectID != ppfdObjectID {
			continue
		}
		// dli owns the clamping and the max-gap guard; this only supplies the
		// sample and the instant it was taken.
		a.deps.Integrator.Add(a.clock.Now(), r.value)
		break
	}
	// Drained on this goroutine, immediately after the Add that produced them:
	// dli calls the handler with its own lock held, so publishing from there
	// would hold the accumulator against the timezone goroutine for a whole
	// publish timeout.
	a.publishRollovers(ctx, a.deps.Rollovers.Drain())
	a.publishDerived(ctx, a.deps.Integrator.Snapshot(), true)
}

// noteValueCount logs the value count a sub-command actually reported, once per
// port session.
//
// This is the missing evidence. The table assumes one value per measurement
// because the Python's parser discarded everything past the first, so nobody
// knows whether 0M! reports PPFD alone or PPFD and detector millivolts
// together. A journal line per session is how the table gets retuned from what
// the sensor does rather than from what the old code implied.
func (a *App) noteValueCount(st *pollState, sub string, n int) {
	if _, seen := st.seen[sub]; seen {
		return
	}
	st.seen[sub] = n
	a.log.Info("measurement value count observed",
		"command", a.measureCommand(sub), "values", n,
		"note", "entities sharing a sub-command share one transaction; retune internal/entities if this is above 1")
}

// recordFailure advances the availability ladder and logs the transition into
// failure — once, on entry, and never again until the next recovery.
//
// Callers must already hold a sequence: it publishes.
func (a *App) recordFailure(ctx context.Context, st *pollState, err error, what string) {
	st.failures++
	if st.failures == 1 {
		st.failingSince = a.clock.Now()
		// From here, sessions counts the port churn this outage costs, which is
		// what the recovery line reports. A healthy daemon's session count is
		// not interesting and would only inflate it.
		st.sessions = 0
		a.log.Warn("sensor readings failing",
			"reason", what, "device", a.cfg.SDI12Port, "error", err)
	}

	// Stashed for the line that marks the outage, which is emitted from
	// forceAvailability several attempts later and has no other way to name the
	// cause.
	a.mu.Lock()
	a.out = outage{reason: what, err: err, attempts: st.failures, since: st.failingSince}
	a.mu.Unlock()

	// An empty retained payload is how a reading is invalidated: grow-app has
	// no time-based staleness whatsoever, so a retained scalar left in place is
	// served as live forever. Never publish "" to mean zero.
	a.blankReadings(ctx)
	if st.failures >= a.cfg.OfflineAfter {
		a.setDegraded(true)
		a.setAvailability(ctx, false)
	}
}

// beat stamps the heartbeat the watchdog reads.
func (a *App) beat() { a.heartbeat.Store(a.clock.Now().UnixNano()) }

// wait blocks for d, returning false if the daemon is stopping instead.
func (a *App) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-a.clock.After(d):
		return true
	}
}

// measureCommand renders the SDI-12 command a sub-command produces, for logs.
func (a *App) measureCommand(sub string) string {
	return fmt.Sprintf("%sM%s!", a.cfg.SDI12Address, sub)
}

// optionalIDs lists the optional entities of a group, for logs.
func optionalIDs(g subGroup) []string {
	var out []string
	for _, e := range g.entities {
		if e.Optional {
			out = append(out, e.ObjectID)
		}
	}
	return out
}

// findEntity looks a row up by object id.
func findEntity(table []entities.Entity, objectID string) (entities.Entity, bool) {
	for _, e := range table {
		if e.ObjectID == objectID {
			return e, true
		}
	}
	return entities.Entity{}, false
}

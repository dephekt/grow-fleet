package app

import (
	"context"
	"slices"
	"strconv"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/mqttpub"
)

// birthReason says what provoked a birth, which decides one thing: whether what
// this process remembers publishing is a fact about the broker in front of it.
type birthReason int

const (
	// birthLocal is a birth this daemon provoked — the probe completed, an
	// entity was retired mid-run, the sensor recovered — over a connection that
	// has been up the whole time. Everything we published is still retained, so
	// only what changed is republished.
	//
	// This is the common case by a wide margin and it is the one that used to
	// flood: a mute-but-enumerated sensor forces a reopen every
	// reopenAfterSoftFailures cycles, each reopen re-probes and re-births, and
	// an unconditional birth then put every discovery config back on the broker
	// several times a minute, indefinitely, for one unplugged sensor wire.
	birthLocal birthReason = iota

	// birthReconnected is a birth from a connection-up. mqttpub creates its
	// session with CleanStart and the broker may have restarted, so nothing we
	// remember publishing can be assumed to still be there and the whole set
	// goes out again. See forgetRetained.
	birthReconnected
)

// publishBirth announces everything this daemon has to say, and is the single
// place discovery is published from.
//
// It runs on two goroutines: the poll goroutine calls it once the probe
// completes and again on recovery or a mid-run retirement, and mqttpub calls it
// from every connection-up sequence. sequence serialises them, because
// interleaving two runs of publishes would let a discovery config land after
// the "online" it is supposed to precede.
//
// The order is discovery, then availability, then values. It matters: a
// subscriber that sees "online" and a state it has no config for registers
// nothing, and grow-app then has an entity id it cannot resolve.
func (a *App) publishBirth(ctx context.Context, reason birthReason) {
	a.sequence(func() { a.birth(ctx, reason) })
}

// birth is publishBirth's body, for callers already inside a sequence.
func (a *App) birth(ctx context.Context, reason birthReason) {
	if ctx.Err() != nil {
		return
	}
	if reason == birthReconnected {
		a.forgetRetained()
	}
	// Change-only for a local birth, because the broker still holds everything
	// this process put there; unconditional after a reconnection, because
	// forgetRetained has just discarded the belief that it does.
	changeOnly := reason == birthLocal

	a.mu.Lock()
	probed := a.probed
	snap := a.snapshot
	working := slices.Clone(a.working)
	unresolved := a.inconclusive
	a.mu.Unlock()

	if !probed {
		// Constraint 1, enforced at the only place it can be. Publishing here
		// is what the Python did in on_connect, and on a pre-3033 unit it
		// announces tilt, which the first poll then retires — leaving a
		// retained config for an entity that never receives a value.
		//
		// The retained availability is still corrected, and it must be: a
		// previous run's "online" left on the broker would have grow-app render
		// its last retained reading as live, and a PAR sensor reporting a
		// constant value looks exactly like night rather than like an outage.
		a.log.Debug("mqtt connected before the capability probe; deferring discovery")
		a.setAvailability(ctx, false)
		return
	}

	dev := a.device
	dev.SWVersion = snap.SWVersion

	// eligible is what *should* be retained after this sequence. It is the set
	// retraction is computed against, and it is derived from the working set
	// rather than from what this process happens to have published.
	eligible := make(map[string]bool, len(working)+len(a.aux))

	for _, e := range working {
		eligible[e.ObjectID] = true
		payload, err := entities.DiscoveryPayload(e, a.topics, dev)
		if err != nil {
			a.log.Error("could not render a discovery config", "object_id", e.ObjectID, "error", err)
			continue
		}
		a.announce(ctx, e.Component, e.ObjectID, payload)
	}
	for _, x := range a.aux {
		eligible[x.ObjectID] = true
		payload, err := x.discoveryPayload(a.topics, dev)
		if err != nil {
			a.log.Error("could not render a discovery config", "object_id", x.ObjectID, "error", err)
			continue
		}
		a.announce(ctx, x.Component, x.ObjectID, payload)
	}

	a.retract(ctx, eligible, unresolved)

	// Availability after discovery, values after availability.
	//
	// The value published is the one the failure ladder currently believes, not
	// an unconditional "online". A birth runs on every reopen as well as on
	// every reconnection, and a sensor that is answering nothing gets reopened
	// every reopenAfterSoftFailures cycles — so announcing "online" here would
	// flap the device card between online and offline for as long as the sensor
	// stayed dead, which is worse than either steady state.
	a.setAvailability(ctx, !a.degradedNow())

	for _, e := range working {
		if e.Kind != entities.SourceStatic {
			continue
		}
		// Eligible guarantees the key is present; a static without a parsed
		// payload is never in the working set in the first place.
		if payload, ok := snap.Statics[e.ObjectID]; ok {
			a.publishState(ctx, e.Component, e.ObjectID, payload, changeOnly)
		}
	}

	// The timezone echo belongs to the birth for the same reason the statics
	// do: grow-app's reconciler decides push-versus-in-sync by comparing our
	// retained state against the value it wants, so a state topic left empty
	// after a broker restart provokes a push we would then have to apply.
	a.publishState(ctx, timeZoneComponent, timeZoneObjectID, a.deps.Zones.State(), changeOnly)

	a.publishDerived(ctx, a.deps.Integrator.Snapshot(), changeOnly)
}

// forgetRetained drops everything this process believes the broker is holding.
//
// mqttpub creates its session with CleanStart and the broker on the far side of
// a new connection may have restarted, so "we already published this" is a fact
// about the connection that just died and not about the one in front of us. A
// daemon that announced only once would be invisible after a broker restart
// until it happened to be restarted itself.
//
// Everything else in this package publishes change-only, and this is what makes
// that safe: the memory those checks consult is reset the moment it stops being
// evidence.
func (a *App) forgetRetained() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.retained)
	// Not lastValues: it is the record of what was *measured*, which a
	// reconnection does not invalidate, and statusLine reports its age. The
	// birth republishes what it owns by passing changeOnly=false instead.
	a.availPublished = false
}

// announce publishes one retained discovery config, skipping the publish when
// the identical payload is already on the broker.
//
// Byte equality is the right test because the payload is deterministic: field
// order is declaration order in both marshallers, and every input is either
// config or the probe's Snapshot. A config that has genuinely changed — a new
// firmware in the device block, a different PPFD unit — differs and goes out.
//
// The skip is not a cosmetic saving. A retained PUBLISH is forwarded to every
// current subscriber regardless of whether the payload changed, so republishing
// an unchanged config wakes grow-app and every other subscriber for nothing;
// see the note on auxPayload.
func (a *App) announce(ctx context.Context, component, objectID string, payload []byte) {
	a.mu.Lock()
	last, known := a.retained[objectID]
	a.mu.Unlock()
	if known && last == string(payload) {
		return
	}

	if err := a.publish(ctx, a.topics.Discovery(component, objectID), payload); err != nil {
		return
	}
	a.mu.Lock()
	a.retained[objectID] = string(payload)
	a.mu.Unlock()
}

// retract clears the retained configs of every entity that is not in the
// eligible set.
//
// A retained message is not self-clearing. When a probe finds tilt unsupported,
// a config published by an earlier port session — or by an earlier *process* —
// outlives the daemon's opinion of the entity forever, and grow-app keeps
// registering an entity whose state topic will never fill again. An empty
// retained payload to the config topic is the retraction.
//
// THE TRAP this closes: the set walked here is the whole table, not the set
// this process remembers announcing. A diff against our own memory is empty on
// the most common path there is — systemd restart, deploy, reboot, sensor swap
// — because a fresh process has announced nothing yet, so a unit probed as
// unsupported would have nothing published about it at all while the previous
// run's config stood on the broker indefinitely. Ineligibility has to be
// asserted, not inferred.
//
// It is asserted once. retained records the empty payload exactly as announce
// records a config, so the reopen loop a mute sensor forces does not republish
// the retraction every session.
//
// THE OTHER TRAP, and the reason unresolved is a separate argument rather than
// folded into eligible: an entity whose probe step *errored* is neither. It is
// not eligible — we have no payload to announce — but the sensor has said
// nothing to justify deleting it either, so the only correct action is none at
// all. Carrying the previous Snapshot's value forward covers this while the
// process has one (see App.adopt); a process that has just started does not, and
// a restart is by far the commonest way to arrive here. A single 0I! timeout on
// the first probe after a reboot would otherwise delete sensor_serial and
// cal_factor from the broker while the journal was still saying, one line above,
// that it was keeping whatever a previous session read.
//
// The state topic is cleared too. Without a config nothing resolves it, so the
// value is inert either way, but leaving the last reading retained on the
// broker means a future config on the same topic silently inherits it.
func (a *App) retract(ctx context.Context, eligible, unresolved map[string]bool) {
	for _, ref := range a.everyEntity() {
		if eligible[ref.objectID] || unresolved[ref.objectID] {
			continue
		}
		a.mu.Lock()
		last, known := a.retained[ref.objectID]
		a.mu.Unlock()
		if known && last == "" {
			continue
		}

		topic := a.topics.Discovery(ref.component, ref.objectID)
		if err := a.publish(ctx, topic, nil); err != nil {
			// Not recorded, so the next birth tries again: it really may still
			// be retained on the broker.
			continue
		}
		// The state clear has to succeed before the retraction is recorded, and
		// for the same reason the config publish does: the guard above skips an
		// entity whose retained payload is already known to be "", so recording
		// first would make a failed state clear permanent. The stale reading
		// would then sit on the state topic for a future config to inherit —
		// exactly what clearing it is for. Re-publishing the (empty) config on
		// the next birth is the cheap half of the pair to repeat.
		if err := a.publishState(ctx, ref.component, ref.objectID, "", true); err != nil {
			continue
		}
		a.mu.Lock()
		a.retained[ref.objectID] = ""
		a.mu.Unlock()

		a.log.Info("retracted a retained discovery config",
			"object_id", ref.objectID, "topic", topic,
			"note", "the entity is not eligible on this unit; a previous process or port session may have announced it")
	}
}

// entityRef is the identity half of a table row: enough to name its topics.
type entityRef struct{ objectID, component string }

// everyEntity lists every object id this daemon can own a retained config for,
// across both tables, in table order so the retraction sequence is stable.
func (a *App) everyEntity() []entityRef {
	out := make([]entityRef, 0, len(a.table)+len(a.aux))
	for _, e := range a.table {
		out = append(out, entityRef{objectID: e.ObjectID, component: e.Component})
	}
	for _, x := range a.aux {
		out = append(out, entityRef{objectID: x.ObjectID, component: x.Component})
	}
	return out
}

// publishReadings writes one cycle's values.
//
// Values that were read are published unconditionally, because
// grow-history-recorder builds a time series out of them and a PAR sensor
// sitting at a constant value overnight is a measurement, not a repeat worth
// suppressing. Blanks are published only on change, because an empty payload
// carries no time and republishing an identical retained message wakes every
// subscriber for nothing.
func (a *App) publishReadings(ctx context.Context, readings []reading) {
	for _, r := range readings {
		if !r.ok {
			a.publishState(ctx, r.entity.Component, r.entity.ObjectID, "", true)
			continue
		}
		a.publishState(ctx, r.entity.Component, r.entity.ObjectID, entities.Format(r.entity, r.value), false)
	}
}

// blankReadings invalidates every polled reading. See recordFailure for why an
// empty payload rather than a zero.
func (a *App) blankReadings(ctx context.Context) {
	for _, e := range a.workingSet() {
		if e.Kind != entities.SourcePolled {
			continue
		}
		a.publishState(ctx, e.Component, e.ObjectID, "", true)
	}
}

// publishDerived writes the DLI accumulator's view.
//
// changeOnly for everything except a reconnection birth: these are derived from
// samples that are already being recorded at full rate, and the ones that move
// slowly — yesterday's total, the peak, the uncredited minutes — would
// otherwise contribute a flat line of identical points all day.
func (a *App) publishDerived(ctx context.Context, r dli.Reading, changeOnly bool) {
	for _, x := range a.aux {
		if x.value == nil {
			continue
		}
		payload := strconv.FormatFloat(x.value(r), 'f', x.Precision, 64)
		a.publishState(ctx, x.Component, x.ObjectID, payload, changeOnly)
	}
}

// publishRollovers writes the midnight sequence dli emitted, in the order it
// emitted it.
//
// The order is the point. RolloverFinal carries the completed day's whole
// total, so the last value recorded against that date is the day; only then
// does the reset publish zeroes. Publishing the reset first would leave the
// finished day's series ending at zero, which reads as a day with no light.
func (a *App) publishRollovers(ctx context.Context, events []dli.Event) {
	for _, e := range events {
		a.log.Info("dli rollover", "kind", e.Kind.String(), "day", e.Day,
			"dli", e.Reading.DLI, "yesterday", e.Reading.Yesterday,
			"photoperiod_hours", e.Reading.PhotoperiodHours, "gap_minutes", e.Reading.GapMinutes)
		a.publishDerived(ctx, e.Reading, true)
	}
}

// setAvailability publishes a retained availability only when it differs from
// the last one we managed to publish, so a device that stays offline says so
// once rather than every cycle. It is the failure ladder's entry point.
//
// availPublished is not redundant with online. App.online starts false, which
// is also what "offline" looks like, so without the second flag the very first
// offline would be suppressed as a no-op — leaving whatever a previous run
// retained, and a stale "online" is the exact state grow-app renders as a live
// sensor reporting a constant value. forgetRetained clears it, which is how a
// reconnection republishes the availability the broker may have forgotten.
func (a *App) setAvailability(ctx context.Context, online bool) {
	a.mu.Lock()
	unchanged := a.availPublished && a.online == online
	a.mu.Unlock()
	if unchanged {
		return
	}
	a.forceAvailability(ctx, online)
}

// forceAvailability publishes the retained availability whether or not it has
// changed, and records what the broker now holds.
//
// The publish and the record are not atomic on their own — the publish takes up
// to publishTimeout and a.mu is not held across it — which is why every caller
// is inside a sequence. Two goroutines racing here could leave "online"
// retained on the broker while App.online records false, and setAvailability
// only publishes on change, so nothing would ever correct it.
func (a *App) forceAvailability(ctx context.Context, online bool) {
	payload := mqttpub.PayloadOffline
	if online {
		payload = mqttpub.PayloadOnline
	}
	if err := a.publish(ctx, a.topics.Status(), []byte(payload)); err != nil {
		return
	}

	a.mu.Lock()
	changed := !a.availPublished || a.online != online
	a.online, a.availPublished = online, true
	out := a.out
	a.mu.Unlock()
	if !changed {
		return
	}
	if online {
		a.log.Info("device is available", "topic", a.topics.Status())
		return
	}
	if out.reason == "" {
		// Not an outage: the failure ladder has never fired in this process, so
		// the only offline this can be is the pre-probe correction birth
		// publishes before it is allowed to announce anything (see App.birth).
		// It is deliberate and it is the healthy path — which is exactly why it
		// must not come out of the line below. A WARN on every restart of a
		// working unit, with nothing filled in but the topic, is what teaches an
		// operator to skip the one line M6 exists to make actionable.
		//
		// Nothing clears out.reason once set, not even a recovery, so this
		// cannot mistake a real outage for a start.
		a.log.Info("cleared a previous run's retained availability; the sensor has not been probed yet",
			"topic", a.topics.Status())
		return
	}
	// Everything needed to act, on the line that marks the outage. In a live run
	// this was the last line before an hour of silence, and it named only an
	// MQTT topic — the actionable detail was in the previous line, which ages
	// out of `journalctl -n`.
	a.log.Warn("device is unavailable; readings are invalidated until it answers again",
		"topic", a.topics.Status(),
		"device", a.cfg.SDI12Port,
		"reason", out.reason,
		"failed_attempts", out.attempts,
		"error", out.err)
}

// degradedNow reports whether the failure ladder has declared the sensor out.
//
// It is tracked separately from App.online because the two answer different
// questions: online is "what is on the status topic", degraded is "what should
// be". They diverge exactly when a birth runs while the sensor is dead — a
// reconnection, or the reopen that a run of transient failures forces — and
// resolving that divergence in favour of the ladder is what stops the device
// card from flapping.
func (a *App) degradedNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.degraded
}

// setDegraded records the ladder's verdict.
func (a *App) setDegraded(degraded bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.degraded = degraded
}

// publishState writes one entity's retained state.
//
// changeOnly suppresses a republish of an identical payload. It is used for the
// values that do not move on a schedule of their own; see publishReadings.
//
// It reports whether the broker holds the payload afterwards — nil for both a
// successful publish and a suppressed no-op, since in either case the value is
// already there. Most callers ignore it because the next cycle republishes
// anyway; App.retract is the one that cannot, because it records a decision
// that suppresses all future attempts.
func (a *App) publishState(ctx context.Context, component, objectID, payload string, changeOnly bool) error {
	a.mu.Lock()
	last, seen := a.lastValues[objectID]
	a.mu.Unlock()
	if changeOnly && seen && last == payload {
		return nil
	}

	topic := a.topics.State(component, objectID)
	if err := a.publish(ctx, topic, []byte(payload)); err != nil {
		return err
	}

	a.mu.Lock()
	a.lastValues[objectID] = payload
	if objectID == ppfdObjectID && payload != "" {
		// Stamped here rather than where the value was read, so the age
		// statusLine prints belongs to the number actually on the bus.
		a.lastReadingAt = a.clock.Now()
	}
	a.mu.Unlock()
	return nil
}

// publish sends one retained QoS 1 message under a bounded budget.
//
// Failures are logged on transition only. A broker that is down fails every
// publish of every cycle, and the Python's habit of logging one line per
// failure per poll forever is as much a defect as the protocol bugs it hid.
//
// It is also the only place in the daemon that observes the broker link at all,
// which is why the outcome is recorded rather than merely logged: statusLine
// has nothing else to report "is the bus up" from.
func (a *App) publish(ctx context.Context, topic string, payload []byte) error {
	pctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	err := a.deps.Publisher.PublishRetained(pctx, topic, payload)

	now := a.clock.Now()
	a.mu.Lock()
	failing := a.publishFailing
	a.publishFailing = err != nil
	a.publishSeen = true
	if err == nil {
		a.lastPublishAt = now
	}
	a.mu.Unlock()

	// A publish is progress. A birth is a couple of dozen of them and a cycle a
	// handful, so a slow broker would otherwise leave the poll goroutine
	// un-stamped for far longer than any single serial transaction — and the
	// watchdog would restart a daemon whose only problem was on the network.
	// Only when this really is the poll goroutine; see pollerCtxKey.
	if onPoller(ctx) {
		a.beat()
	}

	switch {
	case err != nil && !failing:
		a.log.Warn("mqtt publish failing", "topic", topic, "error", err)
	case err == nil && failing:
		a.log.Info("mqtt publish recovered", "topic", topic)
	}
	return err
}

// timeZoneCommandTopic is where grow-app's reconciler pushes a POSIX TZ string.
func (a *App) timeZoneCommandTopic() string {
	return a.topics.Command(timeZoneComponent, timeZoneObjectID)
}

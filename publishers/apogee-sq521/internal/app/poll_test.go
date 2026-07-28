package app

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// probeAndAnnounce runs the two steps runSession runs before its first cycle,
// so a test that only cares about cycles can skip the scheduler.
func (r *testRig) probeAndAnnounce(t *testing.T, ctx context.Context, s Session) {
	t.Helper()
	r.probeAndAnnounceWith(t, ctx, s, &pollState{})
}

// probeAndAnnounceWith is the same, threading a pollState so a test can drive
// several port sessions through one daemon's worth of bookkeeping.
func (r *testRig) probeAndAnnounceWith(t *testing.T, ctx context.Context, s Session, st *pollState) {
	t.Helper()
	r.adopt(r.probe(ctx, s, st))
	r.publishBirth(ctx, birthLocal)
}

// hasTopicPrefix reports whether anything was published under a prefix.
func (p *fakePublisher) hasTopicPrefix(prefix string) bool {
	for _, m := range p.observed() {
		if strings.HasPrefix(m.topic, prefix) {
			return true
		}
	}
	return false
}

// CONSTRAINT 4. The capability probe must complete before any discovery config
// reaches the bus.
//
// entities.Snapshot documents this and cannot enforce it: a zero Snapshot is
// indistinguishable from "nothing has been asked", so Eligible answers "tilt is
// supported" to a caller who never probed. Publishing at MQTT connect — the
// Python's on_connect — therefore announces tilt on a pre-3033 unit, and the
// first poll then retires it, leaving a retained config for an entity whose
// state topic never fills.
//
// The connection is brought up *inside* the probe, at the 0I! call, which is
// the exact interleaving that used to produce the defect.
func TestDiscoveryIsNotPublishedBeforeTheProbeCompletes(t *testing.T) {
	session := newHealthySession()
	// A pre-3033 unit: 0M4! answers with a header declaring zero values.
	session.script("4", step{err: errHeaderZero})

	// Captured on the poll goroutine and handed across, rather than read from
	// the test's goroutine while the poller is still writing.
	seenDuringProbe := make(chan []pubMsg, 1)

	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
	})
	discoveryPrefix := r.cfg.TopicPrefix + "/_discovery"

	session.onCall = func(call string) {
		if call != "I" {
			return
		}
		// autopaho reports the link up in the middle of the probe. The
		// connection-up callback is the one App.Run registered, so this
		// exercises the production wiring rather than a stand-in for it.
		r.pub.connect()
		select {
		case seenDuringProbe <- r.pub.observed():
		default:
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	var duringProbe []pubMsg
	select {
	case duringProbe = <-seenDuringProbe:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe never reached 0I!")
	}
	// The birth ends with the availability, so waiting for "online" is waiting
	// for the whole sequence — and not merely for the "offline" the early
	// connection-up published, which would race the discovery below.
	waitFor(t, "the birth sequence to complete", func() bool {
		got, ok := r.pub.last(r.statusTopic())
		return ok && got == "online" && r.pub.hasTopicPrefix(discoveryPrefix)
	})
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, m := range duringProbe {
		if strings.HasPrefix(m.topic, discoveryPrefix) {
			t.Fatalf("a discovery config was published before the probe finished: %s", m)
		}
	}
	// The connection-up that arrived early must still correct the retained
	// availability, or a previous run's "online" stands and grow-app renders
	// the last retained reading as live.
	if got := payloadsOf(duringProbe, r.statusTopic()); len(got) != 1 || got[0] != "offline" {
		t.Errorf("status published during the probe = %v, want exactly [offline]", got)
	}

	// And once the probe has finished, the announcement is complete and honest.
	if !r.pub.hasTopicPrefix(discoveryPrefix) {
		t.Fatalf("no discovery was published after the probe:\n  %s", formatMsgs(r.pub.observed()))
	}
	// tilt is never *announced*. The empty payload the birth sends is the
	// opposite of an announcement: it clears a config a previous process may
	// have retained on this topic, which is the only way to be sure one is not
	// standing there. See App.retract.
	if got, ok := r.pub.last(r.discoveryTopic("sensor", "tilt")); ok && got != "" {
		t.Errorf("tilt was announced with %q even though the probe found it unsupported", got)
	}
	if got, _ := r.pub.last(r.statusTopic()); got != "online" {
		t.Errorf("status after the birth = %q, want online", got)
	}
}

func payloadsOf(msgs []pubMsg, topic string) []string {
	var out []string
	for _, m := range msgs {
		if m.topic == topic {
			out = append(out, m.payload)
		}
	}
	return out
}

// Discovery must precede the birth "online": a subscriber that sees the
// availability and a state it has no config for registers nothing, which is the
// defect commit 3feb95e fixed in the spectrometer publisher.
func TestBirthPublishesDiscoveryBeforeOnline(t *testing.T) {
	r := newRig(t)
	r.probeAndAnnounce(t, t.Context(), newHealthySession())

	statusAt := r.pub.firstIndex(r.statusTopic())
	if statusAt < 0 {
		t.Fatal("nothing was published to the status topic")
	}
	for _, objectID := range []string{"ppfd", "detector_mv", "tilt", "sensor_serial", "cal_factor"} {
		at := r.pub.firstIndex(r.discoveryTopic("sensor", objectID))
		if at < 0 {
			t.Errorf("%s was never announced", objectID)
			continue
		}
		if at > statusAt {
			t.Errorf("%s discovery was published after the birth online (%d > %d)", objectID, at, statusAt)
		}
	}
	// The statics come last, after the availability that makes them meaningful.
	if at := r.pub.firstIndex(r.stateTopic("sensor", "sensor_serial")); at < statusAt {
		t.Errorf("sensor_serial state was published before the birth online (%d < %d)", at, statusAt)
	}
}

// CONSTRAINT 2. Only a header-declared zero value count retires an entity.
//
// sdi12.ErrNoValues covers two very different facts: the sensor declaring it
// cannot make the measurement, and the sensor promising n values whose data
// response then arrived bare — an adapter swallowing traffic. Retiring on the
// second permanently disables a working channel after one glitch, so the gate
// is sdi12.ErrHeaderNoValues and nothing else.
func TestOnlyAHeaderDeclaredZeroRetiresAnEntity(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantRetired bool
	}{
		{"header declared zero values", errHeaderZero, true},
		{"header promised, data response arrived bare", errDataMissing, false},
		{"malformed frame", errProtocol, false},
		{"silence", errNoResponse, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			session := newHealthySession()
			session.script("4", step{err: tc.err})

			snap := r.probe(t.Context(), session, &pollState{}).snap

			if got := snap.Unsupported["tilt"]; got != tc.wantRetired {
				t.Fatalf("Unsupported[tilt] = %v, want %v", got, tc.wantRetired)
			}
			// The whole point: eligibility, and therefore both discovery and
			// polling, follows from that one bit.
			working := entities.EligibleSet(r.table, snap)
			if got := containsObjectID(working, "tilt"); got == tc.wantRetired {
				t.Errorf("tilt in the working set = %v, want %v", got, !tc.wantRetired)
			}
		})
	}
}

// CONSTRAINT 2, the other half: a required entity is never retired, whatever
// its header says. entities.Eligible ignores Unsupported for non-optional rows,
// but writing the bit anyway would put a falsehood in the Snapshot that a later
// reader could act on.
func TestARequiredEntityIsNeverRetired(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	session.script("", step{err: errHeaderZero}) // 0M! — PPFD, the whole device

	r.probeAndAnnounce(t, t.Context(), session)
	st := &pollState{seen: map[string]int{}}
	r.cycle(t.Context(), session, st)

	r.mu.Lock()
	retired := r.snapshot.Unsupported[ppfdObjectID]
	r.mu.Unlock()
	if retired {
		t.Fatal("ppfd was retired by a zero-value header; a garbled 0M! must never take the primary reading off the bus")
	}
	if !containsObjectID(r.workingSet(), ppfdObjectID) {
		t.Error("ppfd left the working set")
	}
}

// CONSTRAINT 5. A retained discovery config is not self-clearing, so a probe
// that flips tilt to unsupported *after* it has been announced has to retract
// the config explicitly. Otherwise grow-app keeps registering an entity whose
// state topic will never fill again.
func TestMidRunRetirementRetractsTheRetainedConfig(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()

	// Announced as supported, and only later does the sensor decline it — the
	// shape of a unit swapped for an older one, or a firmware downgrade.
	r.probeAndAnnounce(t, t.Context(), session)
	tiltConfig := r.discoveryTopic("sensor", "tilt")
	if _, ok := r.pub.last(tiltConfig); !ok {
		t.Fatal("tilt was never announced, so there is nothing to retract")
	}

	r.pub.reset()
	session.script("4", step{err: errHeaderZero})
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})

	got, ok := r.pub.last(tiltConfig)
	if !ok {
		t.Fatalf("nothing was published to %s:\n  %s", tiltConfig, formatMsgs(r.pub.observed()))
	}
	if got != "" {
		t.Errorf("retraction payload = %q, want the empty payload that clears a retained message", got)
	}
	// Dropped from polling as well as from discovery: one funnel, not two.
	if containsObjectID(r.workingSet(), "tilt") {
		t.Error("tilt is still in the working set after being retired")
	}
	r.pub.reset()
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})
	for _, m := range r.pub.observed() {
		if strings.Contains(m.topic, "/tilt/") {
			t.Errorf("tilt was still serviced after retirement: %s", m)
		}
	}
}

// CONSTRAINT 5, the case that reaches it: a transient probe failure must never
// retract an entity that was previously established.
//
// probe omits a static from Statics on *any* Identify/Verify error, adopt
// replaces the whole Snapshot, EligibleSet drops the entity and the birth
// retracts it — so one 0I!/0V! timeout deleted two retained configs and their
// state topics. The timing is what makes it more than theoretical: the reopen
// that re-probes fires at reopenAfterSoftFailures, i.e. exactly when the sensor
// is flaky and a probe step is most likely to time out, and the entities then
// stay retracted until some later reopen whose probe happens to succeed.
//
// probeCapabilities already keyed strictly on ErrHeaderNoValues. This is the
// same rule on the path that actually deletes.
func TestATransientProbeFailureDoesNotRetractAnEstablishedEntity(t *testing.T) {
	r := newRig(t)
	st := &pollState{}
	r.probeAndAnnounceWith(t, t.Context(), newHealthySession(), st)

	// Session 2, on the reopen a flaky sensor forces: both identification
	// commands time out.
	r.pub.reset()
	flaky := newHealthySession()
	flaky.identErr = errNoResponse
	flaky.verifyErr = errNoResponse
	r.probeAndAnnounceWith(t, t.Context(), flaky, st)

	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		for _, topic := range []string{
			r.discoveryTopic("sensor", objectID),
			r.stateTopic("sensor", objectID),
		} {
			if got, ok := r.pub.last(topic); ok && got == "" {
				t.Errorf("a timed-out probe step deleted %s", topic)
			}
		}
		if !containsObjectID(r.workingSet(), objectID) {
			t.Errorf("%s left the working set after an inconclusive probe", objectID)
		}
	}

	// The values themselves survive, so the state topics keep saying something
	// true rather than going blank.
	r.mu.Lock()
	statics := maps.Clone(r.snapshot.Statics)
	swVersion := r.snapshot.SWVersion
	r.mu.Unlock()
	if got := statics[serialObjectID]; got != "3043" {
		t.Errorf("sensor_serial = %q after an inconclusive identify, want the value the first session read", got)
	}
	if got := statics[calFactorObjectID]; got != "1.23457" {
		t.Errorf("cal_factor = %q after an inconclusive verify, want the value the first session read", got)
	}
	// sw_version too, or every discovery config's device block changes on a
	// flaky reopen and the whole set is republished for no new information.
	if swVersion != "104" {
		t.Errorf("sw_version = %q after an inconclusive identify, want 104", swVersion)
	}
}

// The same rule on a *restart*, which is where it actually bites.
//
// The carry-forward above is a property of the Snapshot being replaced, so it is
// gated on a.probed — and a.probed is false in a fresh process. A restart whose
// very first probe timed out on 0I!/0V! therefore had nothing to carry forward
// and retracted both statics outright: discovery configs and state topics,
// deleted from the broker by a sensor that merely failed to answer once.
//
// The path is the common one, not a contrived one. systemd restarts, deploys and
// reboots all land here, and a flaky sensor is precisely why someone restarts the
// daemon in the first place. The journal then contradicted itself two lines
// apart — "keeping whatever a previous session read" directly above "retracted a
// retained discovery config object_id=sensor_serial" — which is the 2am hazard
// this whole distinction exists to remove.
//
// probeResult.inconclusive already knows the difference between "the step
// errored" and "the sensor answered no". Only the second may retract, in every
// process lifetime.
func TestATransientProbeFailureOnAFreshProcessDoesNotRetract(t *testing.T) {
	// Process 1 puts both statics on the broker.
	first := newRig(t)
	first.probeAndAnnounce(t, t.Context(), newHealthySession())
	broker := first.pub
	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		if got, ok := broker.last(first.discoveryTopic("sensor", objectID)); !ok || got == "" {
			t.Fatalf("process 1 never announced %s; there is nothing for process 2 to preserve", objectID)
		}
	}

	// Process 2: a fresh App against the same broker — it has probed nothing and
	// remembers nothing — whose first probe times out on both identification
	// commands.
	second := newRig(t, withJournal(t), func(r *testRig) { r.pub = broker })
	broker.reset()
	flaky := newHealthySession()
	flaky.identErr = errNoResponse
	flaky.verifyErr = errNoResponse
	second.probeAndAnnounce(t, t.Context(), flaky)

	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		for _, topic := range []string{
			second.discoveryTopic("sensor", objectID),
			second.stateTopic("sensor", objectID),
		} {
			if got, ok := broker.last(topic); ok && got == "" {
				t.Errorf("a timed-out probe step on a fresh process deleted %s", topic)
			}
		}
	}

	// And the journal says one thing rather than both.
	for _, line := range second.journal.matching("retracted a retained discovery config") {
		for _, objectID := range []string{serialObjectID, calFactorObjectID} {
			if strings.Contains(line, "object_id="+objectID) {
				t.Errorf("the journal contradicts itself two lines apart:\n%s", second.journal.dump())
			}
		}
	}
}

// The complement, and the reason the fix is not simply "never retract": a
// sensor that *answers* and says it has no calibration coefficient is a
// definitive negative, and the config must go.
func TestADefinitiveNegativeStillRetracts(t *testing.T) {
	r := newRig(t)
	st := &pollState{}
	r.probeAndAnnounceWith(t, t.Context(), newHealthySession(), st)

	r.pub.reset()
	answered := newHealthySession()
	// 0V! replies, declaring zero coefficients. sdi12 reports that as an error
	// wrapping ErrHeaderNoValues, never as (nil, nil) — an earlier version of
	// this test set verify = nil, which production cannot produce, so it passed
	// against a dead branch while the real definitive-negative path was filed as
	// inconclusive and cal_factor was never retracted in any process lifetime.
	answered.verify, answered.verifyErr = nil, errVerifyHeaderZero
	r.probeAndAnnounceWith(t, t.Context(), answered, st)

	config := r.discoveryTopic("sensor", calFactorObjectID)
	got, ok := r.pub.last(config)
	if !ok || got != "" {
		t.Errorf("cal_factor config = %q (published=%v), want the empty payload; the sensor answered that it has none",
			got, ok)
	}
	if containsObjectID(r.workingSet(), calFactorObjectID) {
		t.Error("cal_factor is still in the working set after a definitive negative")
	}
}

// HIGH 5, the journal half. The probe runs once per port session, and a mute
// sensor forces a session every reopenAfterSoftFailures cycles forever — so an
// unguarded warning here is four lines per session, thousands a day, and
// `journalctl -n 200` returns two hundred identical probe warnings and never
// reaches the line that says when the trouble started.
func TestTheProbeDoesNotRepeatItsWarningsEveryPortSession(t *testing.T) {
	session := newHealthySession()
	session.identErr = errNoResponse
	session.verifyErr = errNoResponse
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	r := newRig(t, withJournal(t), func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()
	for range 20 {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(r.cfg.PollInterval)
	}
	waitFor(t, "four port sessions", func() bool { return r.dialer.dials() >= 4 })
	cancel()
	<-done

	sessions := r.dialer.dials()
	if sessions < 4 {
		t.Fatalf("only %d port sessions; this test needs the reopen loop", sessions)
	}
	// One line each, for the whole outage, however many sessions it churns
	// through.
	for _, line := range []string{
		"sensor identification failed",
		"calibration read (0V!) failed",
		"optional measurement probe inconclusive",
		"consecutive transient sensor failures",
		"sensor readings failing",
	} {
		if n := r.journal.count(line); n > 1 {
			t.Errorf("%q appears %d times across %d port sessions, want at most 1:\n%s",
				line, n, sessions, r.journal.dump())
		}
	}
	// And the onset is still there to be found, which is the whole point.
	if r.journal.count("sensor readings failing") != 1 {
		t.Errorf("the line marking the onset is missing:\n%s", r.journal.dump())
	}
}

// M6. The line that marks the outage has to carry something an operator can act
// on. In a live run it was the last line before an hour of silence, and it
// named only an MQTT topic — the actionable detail was in the previous line,
// which ages out of `journalctl -n`.
func TestTheOutageLineNamesSomethingActionable(t *testing.T) {
	r := newRig(t, withJournal(t))
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}

	st := &pollState{seen: map[string]int{}}
	for range r.cfg.OfflineAfter {
		r.cycle(t.Context(), session, st)
	}

	lines := r.journal.matching("device is unavailable")
	if len(lines) != 1 {
		t.Fatalf("the outage was marked %d times, want once:\n%s", len(lines), r.journal.dump())
	}
	for _, want := range []string{
		r.cfg.SDI12Port,            // which device
		"reason=",                  // what failed
		"failed_attempts=3",        // how many times
		"read timed out",           // the error itself
		"topic=" + r.statusTopic(), // and what was published about it
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the outage line does not mention %q:\n  %s", want, lines[0])
		}
	}
}

// M5. The retry gap doubles from dialBackoffMin to dialBackoffMax, so a single
// line at the first failure states a number that is wrong for every retry after
// it. An operator re-seats the adapter, reads "retry_in=1s", sees nothing for
// thirty seconds and restarts the daemon believing it wedged.
func TestTheDialBackoffScheduleIsReportedAccurately(t *testing.T) {
	r := newRig(t, withJournal(t), func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) {
			return nil, errors.New("resolve /dev/serial/by-id/…: no such file or directory")
		}}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	// Far enough for the backoff to climb from 1 s to the 30 s ceiling.
	for range 12 {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(dialBackoffMax)
	}
	cancel()
	<-done

	first := r.journal.matching("could not open the serial device")
	if len(first) != 1 {
		t.Fatalf("the first open failure was reported %d times, want once:\n%s", len(first), r.journal.dump())
	}
	if !strings.Contains(first[0], "retry_in="+dialBackoffMin.String()) {
		t.Errorf("the first line does not state the gap in force:\n  %s", first[0])
	}
	if !strings.Contains(first[0], dialBackoffMax.String()) {
		t.Errorf("the first line does not say the gap grows, so its number reads as permanent:\n  %s", first[0])
	}

	ceiling := r.journal.matching("still absent")
	if len(ceiling) != 1 {
		t.Fatalf("the arrival at the backoff ceiling was reported %d times, want exactly once:\n%s",
			len(ceiling), r.journal.dump())
	}
	if !strings.Contains(ceiling[0], "retry_in="+dialBackoffMax.String()) {
		t.Errorf("the ceiling line does not state the gap now in force:\n  %s", ceiling[0])
	}
}

// The failure ladder, both rungs and the recovery.
//
// An empty retained payload is how a reading is invalidated. grow-app has no
// time-based staleness at all, so a retained scalar left in place is served as
// live forever — which for a PAR sensor looks like a steady value rather than
// like an outage. Availability is the slower signal: one flaky read must not
// flap the device card, so it takes cfg.OfflineAfter cycles.
func TestFailureLadderBlanksReadingsThenGoesOffline(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)

	ppfdState := r.stateTopic("sensor", ppfdObjectID)
	st := &pollState{seen: map[string]int{}}

	// Everything stops answering.
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}

	r.pub.reset()
	r.cycle(t.Context(), session, st) // cycle 1
	if got := r.pub.payloads(ppfdState); len(got) != 1 || got[0] != "" {
		t.Errorf("cycle 1 published %v to %s, want exactly [\"\"]", got, ppfdState)
	}
	if got := r.pub.payloads(r.statusTopic()); len(got) != 0 {
		t.Errorf("cycle 1 touched availability (%v); one failed read must not flap the device card", got)
	}

	r.cycle(t.Context(), session, st) // cycle 2
	if got := r.pub.payloads(r.statusTopic()); len(got) != 0 {
		t.Errorf("cycle 2 touched availability (%v), want nothing until OfflineAfter", got)
	}
	// The retained "" is already in place; republishing an identical retained
	// message wakes every subscriber for no information.
	if got := r.pub.payloads(ppfdState); len(got) != 1 {
		t.Errorf("cycle 2 republished the blank reading (%d publishes), want it suppressed", len(got))
	}

	r.cycle(t.Context(), session, st) // cycle 3 == OfflineAfter
	if got := r.pub.payloads(r.statusTopic()); len(got) != 1 || got[0] != "offline" {
		t.Fatalf("cycle 3 published %v to the status topic, want exactly [offline]", got)
	}

	r.cycle(t.Context(), session, st) // cycle 4: still offline, still silent
	if got := r.pub.payloads(r.statusTopic()); len(got) != 1 {
		t.Errorf("availability was republished while nothing changed (%v)", got)
	}

	// Recovery: availability, then the fresh value.
	//
	// The discovery config is deliberately NOT republished. It went out on this
	// same connection when the probe completed and the broker still holds it —
	// republishing an identical retained message forwards it to every current
	// subscriber for no information, and a mute sensor recovers and fails often
	// enough for that to matter. The ordering constraint is satisfied by the
	// config that is already there.
	r.pub.reset()
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{vals: []float64{42}})
	}
	r.cycle(t.Context(), session, st)

	statusAt := r.pub.firstIndex(r.statusTopic())
	valueAt := r.pub.firstIndex(ppfdState)
	switch {
	case statusAt < 0:
		t.Error("recovery did not republish the availability")
	case valueAt < 0:
		t.Error("recovery did not publish a fresh value")
	case statusAt > valueAt:
		t.Errorf("recovery order was status=%d value=%d, want the availability first", statusAt, valueAt)
	}
	if at := r.pub.firstIndex(r.discoveryTopic("sensor", ppfdObjectID)); at >= 0 {
		t.Errorf("recovery republished an unchanged discovery config at %d:\n  %s",
			at, formatMsgs(r.pub.observed()))
	}
	if got, _ := r.pub.last(r.statusTopic()); got != "online" {
		t.Errorf("status after recovery = %q, want online", got)
	}
	if got, _ := r.pub.last(ppfdState); got != "42.0" {
		t.Errorf("ppfd after recovery = %q, want 42.0", got)
	}
}

// A cycle in which some transactions fail and others succeed is not a device
// outage: the entity that got no value blanks, and availability is untouched.
func TestAPartialFailureBlanksOnlyItsOwnEntity(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)

	r.pub.reset()
	session.script("1", step{err: errNoResponse}) // detector_mv only
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})

	if got, _ := r.pub.last(r.stateTopic("sensor", "detector_mv")); got != "" {
		t.Errorf("detector_mv = %q, want the empty payload", got)
	}
	if got, _ := r.pub.last(r.stateTopic("sensor", ppfdObjectID)); got != "156.9" {
		t.Errorf("ppfd = %q, want 156.9", got)
	}
	if got := r.pub.payloads(r.statusTopic()); len(got) != 0 {
		t.Errorf("availability changed on a partial failure (%v)", got)
	}
}

// ErrPortDead abandons the cycle where it happens, so every group after the
// failing one is never asked — and those entities still need their retained
// readings invalidated.
//
// The trap is that a cycle with one successful transaction counts as a success,
// so recordFailure and its blankReadings never run. Nothing else would clear
// them, and grow-app has no time-based staleness whatsoever: the last tilt
// reading would be served as live for the whole reopen.
func TestPortDeadBlanksTheGroupsItNeverAsked(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})

	tiltState := r.stateTopic("sensor", "tilt")
	if got, _ := r.pub.last(tiltState); got != "1.20" {
		t.Fatalf("tilt = %q, want a reading for the dead port to invalidate", got)
	}

	// ppfd (0M!) answers, then the port dies on detector_mv (0M1!). tilt (0M4!)
	// is behind it in the table and is never reached.
	r.pub.reset()
	before := len(session.commands())
	session.script("1", step{err: errPortDead})
	if got := r.cycle(t.Context(), session, &pollState{seen: map[string]int{}}); got != outcomeReopen {
		t.Fatalf("cycle = %v, want outcomeReopen", got)
	}
	if got := strings.Join(session.commands()[before:], ","); got != "M,M1" {
		t.Fatalf("the dead cycle sent %q, want it to stop at 0M1!; this test proves nothing otherwise", got)
	}

	if got, ok := r.pub.last(tiltState); !ok || got != "" {
		t.Errorf("tilt = %q (published=%v), want the empty payload: its transaction never ran", got, ok)
	}
	if got, _ := r.pub.last(r.stateTopic("sensor", "detector_mv")); got != "" {
		t.Errorf("detector_mv = %q, want the empty payload", got)
	}
}

// Error routing, one row per sdi12 sentinel. The recovery action differs per
// class, and conflating them is what left the Python's desynced port desynced
// forever.
func TestErrorRoutingPerSentinel(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome cycleOutcome
		wantRetired bool
		// wantSoft is whether the failure counts toward the reopen escalation.
		wantSoft bool
	}{
		{
			name:        "port dead reopens immediately",
			err:         errPortDead,
			wantOutcome: outcomeReopen,
		},
		{
			name:        "malformed frame is transient; sdi12 already drained",
			err:         errProtocol,
			wantOutcome: outcomeContinue,
			wantSoft:    true,
		},
		{
			name:        "silence is transient",
			err:         errNoResponse,
			wantOutcome: outcomeContinue,
			wantSoft:    true,
		},
		{
			name:        "a lost data response is transient, never a retirement",
			err:         errDataMissing,
			wantOutcome: outcomeContinue,
			wantSoft:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			session := newHealthySession()
			r.probeAndAnnounce(t, t.Context(), session)
			for _, sub := range []string{"", "1", "4"} {
				session.script(sub, step{err: tc.err})
			}

			st := &pollState{seen: map[string]int{}}
			got := r.cycle(t.Context(), session, st)

			if got != tc.wantOutcome {
				t.Errorf("cycle outcome = %v, want %v", got, tc.wantOutcome)
			}
			if want := 1; tc.wantSoft && st.soft != want {
				t.Errorf("soft failures = %d, want %d", st.soft, want)
			}
			if !tc.wantSoft && st.soft != 0 {
				t.Errorf("soft failures = %d, want 0", st.soft)
			}
			r.mu.Lock()
			retired := len(r.snapshot.Unsupported) > 0
			r.mu.Unlock()
			if retired != tc.wantRetired {
				t.Errorf("entities retired = %v, want %v", retired, tc.wantRetired)
			}
		})
	}
}

// Three cycles of nothing usable is no longer transient. The adapter's MCU
// reset is the only remaining lever, and it is the one the Python only ever
// pulled for an OSError.
func TestConsecutiveTransientFailuresEscalateToAReopen(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errProtocol})
	}

	st := &pollState{seen: map[string]int{}}
	for i := 1; i < reopenAfterSoftFailures; i++ {
		if got := r.cycle(t.Context(), session, st); got != outcomeContinue {
			t.Fatalf("cycle %d outcome = %v, want outcomeContinue", i, got)
		}
	}
	if got := r.cycle(t.Context(), session, st); got != outcomeReopen {
		t.Fatalf("cycle %d outcome = %v, want outcomeReopen", reopenAfterSoftFailures, got)
	}
}

// A dead descriptor closes and reopens, and the reopen re-probes: sdi12's
// LineReader holds bytes framed off the old fd, so carrying a client across the
// reopen would start the new session mid-frame.
func TestPortDeadClosesAndReopens(t *testing.T) {
	first := newHealthySession()
	for _, sub := range []string{"", "1", "4"} {
		first.script(sub, step{err: errPortDead})
	}
	second := newHealthySession()

	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(n int) (Session, error) {
			if n == 0 {
				return first, nil
			}
			return second, nil
		}}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	// The second session gets its own probe before its first cycle.
	waitFor(t, "the second session to be probed", func() bool {
		return len(second.commands()) >= 3
	})
	cancel()
	<-done

	if first.closeCount() != 1 {
		t.Errorf("the dead session was closed %d times, want 1", first.closeCount())
	}
	if got := second.commands(); got[0] != "I" || got[1] != "V" {
		t.Errorf("the reopened session started with %v, want a fresh identify and verify", got[:2])
	}
	if r.dialer.dials() < 2 {
		t.Errorf("dials = %d, want at least 2", r.dialer.dials())
	}
}

// CONSTRAINT 1, stated as arithmetic. sdi12 refuses to send aD0! when the
// caller's context truncated the sensor's conversion wait — correctly, because
// proceeding puts a data command on the bus mid-measurement. A budget sized
// from POLL_SECONDS rather than from ttt therefore fails *every* cycle.
func TestCycleBudgetOutlastsTheSensorsConversionWait(t *testing.T) {
	cfg := testConfig()
	// The measured boundary: with ttt=001 a 1.9 s budget returns
	// context.DeadlineExceeded and a 2.1 s budget returns the value.
	const declaredTTT = time.Second
	perTransaction := transactionBudget(cfg.ReadTimeout)
	if perTransaction <= declaredTTT+sdi12ServiceRequestSlack {
		t.Fatalf("transaction budget %s does not exceed ttt+slack %s; sdi12 would refuse every aD0!",
			perTransaction, declaredTTT+sdi12ServiceRequestSlack)
	}

	table := entities.Build(entities.Params{PPFDUnit: cfg.PPFDUnit})
	groups := groupBySubCmd(table)
	if got, want := cycleBudget(table, cfg.ReadTimeout), time.Duration(len(groups))*perTransaction; got != want {
		t.Errorf("cycle budget = %s, want one transaction budget per sub-command (%s)", got, want)
	}
	// And it must be sized from the sensor rather than from the poll interval,
	// which is the mistake the constant exists to prevent.
	if cycleBudget(table, cfg.ReadTimeout) <= cfg.PollInterval {
		t.Errorf("cycle budget %s is no larger than POLL_SECONDS %s; it is meant to follow ttt, not the cadence",
			cycleBudget(table, cfg.ReadTimeout), cfg.PollInterval)
	}
}

// Our own budget expiring is a configuration fault, not a wire outcome, and it
// gets exactly one line rather than one per cycle.
func TestABudgetOverrunIsNotTreatedAsASensorFault(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: context.DeadlineExceeded})
	}

	st := &pollState{seen: map[string]int{}}
	if got := r.cycle(t.Context(), session, st); got != outcomeContinue {
		t.Errorf("cycle outcome = %v, want outcomeContinue", got)
	}
	if st.soft != 0 {
		t.Errorf("soft failures = %d; a truncated budget is ours, not the adapter's, so it must not provoke a reopen", st.soft)
	}
	if !st.budgetWarned {
		t.Error("the budget overrun was not reported; it is the only signal that maxConversionWait is too small")
	}
}

// Absolute-boundary scheduling. The Python slept a flat POLL_SECONDS *after*
// three serial transactions, so a 10 s setting produced a real 13-16 s cadence
// and every retry added itself to the period permanently.
func TestAnOverrunningCycleSkipsBoundariesRatherThanDrifting(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()

	start := r.clock.Now()
	// The first cycle takes 25 s of a 10 s cadence.
	session.onMeasure = func(sub string, n int) {
		if sub == "" && n == 0 {
			r.clock.Advance(25 * time.Second)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runSession(ctx, session, &pollState{}) }()

	r.clock.awaitWaiters(t, 1)
	pending := r.clock.pending()
	if len(pending) != 1 {
		t.Fatalf("pending timers = %d, want 1", len(pending))
	}

	// The two boundaries at +10 s and +20 s are gone; the next one on the grid
	// is +30 s. A flat post-work sleep would have parked until +35 s, and the
	// error would compound on every subsequent cycle.
	want := start.Add(30 * time.Second)
	if !pending[0].Equal(want) {
		t.Errorf("next wake at %s, want the absolute boundary %s (a flat sleep would give %s)",
			pending[0].UTC(), want.UTC(), start.Add(35*time.Second).UTC())
	}

	cancel()
	<-done
}

func TestNextBoundary(t *testing.T) {
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Second

	tests := []struct {
		name        string
		elapsed     time.Duration
		wantAt      time.Duration
		wantSkipped int
	}{
		{"cycle finished early", 3 * time.Second, 10 * time.Second, 0},
		{"cycle finished exactly on the boundary", 10 * time.Second, 10 * time.Second, 0},
		{"cycle overran by half an interval", 15 * time.Second, 20 * time.Second, 1},
		{"cycle overran by exactly one interval", 20 * time.Second, 20 * time.Second, 1},
		{"cycle overran by two and a half", 25 * time.Second, 30 * time.Second, 2},
		{"cycle took a whole minute", 60 * time.Second, 60 * time.Second, 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at, skipped := nextBoundary(base, interval, base.Add(tc.elapsed))
			if want := base.Add(tc.wantAt); !at.Equal(want) {
				t.Errorf("next boundary = %s, want %s", at.UTC(), want.UTC())
			}
			if skipped != tc.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tc.wantSkipped)
			}
			// The invariant every row shares: the answer is never in the past,
			// and it is always on the grid.
			if at.Before(base.Add(tc.elapsed)) {
				t.Errorf("next boundary %s is before now %s", at.UTC(), base.Add(tc.elapsed).UTC())
			}
			if at.Sub(base)%interval != 0 {
				t.Errorf("next boundary %s is not a whole number of intervals past %s", at.UTC(), base.UTC())
			}
		})
	}
}

// Transactions, not entities. If 0M!'s header ever reports two values, PPFD and
// detector millivolts collapse into one conversation — but the table is not
// pre-collapsed on that guess, so today this is one group per row.
func TestGroupBySubCmd(t *testing.T) {
	table := entities.Build(entities.Params{PPFDUnit: "µmol/s/m²"})
	groups := groupBySubCmd(table)

	var subs []string
	for _, g := range groups {
		subs = append(subs, g.sub)
	}
	if want := []string{"", "1", "4"}; !equalStrings(subs, want) {
		t.Errorf("sub-commands = %v, want %v (table order)", subs, want)
	}
	for _, g := range groups {
		for _, e := range g.entities {
			if e.Kind != entities.SourcePolled {
				t.Errorf("%s is %v but was grouped as polled", e.ObjectID, e.Kind)
			}
			if e.SubCmd != g.sub {
				t.Errorf("%s has sub-command %q but landed in group %q", e.ObjectID, e.SubCmd, g.sub)
			}
		}
	}

	// Two entities on one sub-command must share one transaction and read
	// different indices out of the same response.
	shared := []entities.Entity{
		{ObjectID: "a", Kind: entities.SourcePolled, SubCmd: "", Index: 0},
		{ObjectID: "b", Kind: entities.SourcePolled, SubCmd: "", Index: 1},
		{ObjectID: "c", Kind: entities.SourceStatic},
	}
	if got := groupBySubCmd(shared); len(got) != 1 || len(got[0].entities) != 2 {
		t.Errorf("two entities on one sub-command produced %d groups, want 1 group of 2", len(got))
	}
}

// Values are routed by Index, and an index past the end of a short response is
// a missing reading rather than a panic.
func TestValuesAreRoutedByIndex(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	session.script("", step{vals: []float64{1.5, 2.5}})

	table := []entities.Entity{
		{ObjectID: "first", Component: "sensor", Kind: entities.SourcePolled, SubCmd: "", Index: 0,
			PayloadFormat: entities.FormatFixed, PayloadPrecision: 1},
		{ObjectID: "second", Component: "sensor", Kind: entities.SourcePolled, SubCmd: "", Index: 1,
			PayloadFormat: entities.FormatFixed, PayloadPrecision: 1},
		{ObjectID: "third", Component: "sensor", Kind: entities.SourcePolled, SubCmd: "", Index: 2,
			PayloadFormat: entities.FormatFixed, PayloadPrecision: 1},
	}
	readings, res := r.measure(t.Context(), session, groupBySubCmd(table), &pollState{seen: map[string]int{}})

	if res.ok != 1 {
		t.Fatalf("successful transactions = %d, want 1", res.ok)
	}
	want := map[string]struct {
		value float64
		ok    bool
	}{
		"first":  {1.5, true},
		"second": {2.5, true},
		"third":  {0, false},
	}
	for _, got := range readings {
		w := want[got.entity.ObjectID]
		if got.ok != w.ok || (got.ok && got.value != w.value) {
			t.Errorf("%s = (%v, ok=%v), want (%v, ok=%v)", got.entity.ObjectID, got.value, got.ok, w.value, w.ok)
		}
	}
}

// The probe is per port session, not per cycle: three commands at the open and
// then nothing but measurements.
func TestTheProbeRunsOncePerPortSession(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()

	r.probeAndAnnounce(t, t.Context(), session)
	st := &pollState{seen: map[string]int{}}
	for range 3 {
		r.cycle(t.Context(), session, st)
	}

	var identifies, verifies int
	for _, c := range session.commands() {
		switch c {
		case "I":
			identifies++
		case "V":
			verifies++
		}
	}
	if identifies != 1 || verifies != 1 {
		t.Errorf("identify=%d verify=%d across three cycles, want 1 and 1", identifies, verifies)
	}
}

// Identification is best effort. A sensor that will not say who it is still
// measures, and the statics simply go unannounced rather than being announced
// with a state topic that never fills.
func TestAFailedProbeStepOmitsItsEntitiesRatherThanFailing(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	session.identErr = errNoResponse
	session.verifyErr = errNoResponse

	r.probeAndAnnounce(t, t.Context(), session)

	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		// Only the retraction is allowed on these topics: an empty payload
		// clears whatever a previous process left retained there, which is not
		// an announcement.
		if got, ok := r.pub.last(r.discoveryTopic("sensor", objectID)); ok && got != "" {
			t.Errorf("%s was announced with %q despite having no parsed value", objectID, got)
		}
	}
	if _, ok := r.pub.last(r.discoveryTopic("sensor", ppfdObjectID)); !ok {
		t.Error("ppfd was not announced; a failed identification must not stop the measurement entities")
	}
	// sw_version is simply omitted from the device block, as the Python did by
	// only setting the key when it parsed one.
	payload, _ := r.pub.last(r.discoveryTopic("sensor", ppfdObjectID))
	if strings.Contains(payload, "sw_version") {
		t.Errorf("sw_version appears in the device block after a failed identify: %s", payload)
	}
}

// PPFD, and only PPFD, feeds the integrator — and only when it was really read.
func TestOnlyRealPPFDReachesTheIntegrator(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	st := &pollState{seen: map[string]int{}}

	r.cycle(t.Context(), session, st)
	if got := r.acc.added(); len(got) != 1 || got[0].ppfd != 156.9289 {
		t.Fatalf("samples = %+v, want one sample of 156.9289", got)
	}

	session.script("", step{err: errNoResponse})
	r.cycle(t.Context(), session, st)
	if got := r.acc.added(); len(got) != 1 {
		t.Errorf("samples = %+v; a failed read must not be integrated as a value", got)
	}
}

func containsObjectID(table []entities.Entity, objectID string) bool {
	for _, e := range table {
		if e.ObjectID == objectID {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitFor polls cond until it holds, failing the test if it never does. Used
// only where a real goroutine has to reach a state the fake clock cannot force.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// USB enumeration routinely lags a Pi reboot by seconds and systemd may well
// start us first, so a device that is not there yet is a state to retry
// through, not a reason to exit. It is also the window in which the Python had
// no will registered at all, having spent 36 seconds on this loop *before*
// configuring MQTT.
func TestADeviceThatIsNotThereYetIsRetriedWithBackoff(t *testing.T) {
	var absent = errors.New("resolve /dev/serial/by-id/…: no such file or directory")
	session := newHealthySession()

	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(n int) (Session, error) {
			if n < 3 {
				return nil, absent
			}
			return session, nil
		}}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	// The gap doubles from dialBackoffMin, and the poller parks on it rather
	// than spinning on the bus.
	want := dialBackoffMin
	for range 3 {
		r.clock.awaitWaiters(t, 1)
		at := r.clock.pending()[0]
		if got := at.Sub(r.clock.Now()); got != want {
			t.Errorf("retry gap = %s, want %s", got, want)
		}
		r.clock.Advance(want)
		want = min(2*want, dialBackoffMax)
	}

	// And once the adapter appears, the session runs normally.
	waitFor(t, "the sensor to be probed", func() bool { return len(session.commands()) >= 3 })
	waitFor(t, "the device to be marked offline while it was absent", func() bool {
		return len(r.pub.payloads(r.statusTopic())) > 0
	})
	if got := r.pub.payloads(r.statusTopic())[0]; got != "offline" {
		t.Errorf("the first availability publish while the adapter was missing = %q, want offline", got)
	}

	cancel()
	<-done
}

// Cancelling during the backoff must return at the next select, not wait the
// gap out. The Python's equivalent held SIGTERM for up to 36 seconds.
func TestADialBackoffIsInterruptedByShutdown(t *testing.T) {
	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) {
			return nil, errors.New("no such device")
		}}
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	r.clock.awaitWaiters(t, 1) // parked on the backoff, with no clock to fire it
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the poll goroutine did not return until its backoff elapsed")
	}
}

package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
)

// New must refuse a Deps with a hole in it rather than panicking on the first
// use. Every collaborator is load-bearing, and a nil one would surface as a
// crash in the poll goroutine on a Pi at 3 a.m.
func TestNewRequiresEveryCollaborator(t *testing.T) {
	full := func() Deps {
		return Deps{
			Dialer:     &fakeDialer{fn: func(int) (Session, error) { return newHealthySession(), nil }},
			Publisher:  newFakePublisher(),
			Notifier:   &fakeNotifier{},
			Zones:      &fakeZones{loc: time.UTC},
			Integrator: &fakeIntegrator{},
			Rollovers:  &RolloverSink{},
		}
	}

	tests := map[string]func(*Deps){
		"Dialer":     func(d *Deps) { d.Dialer = nil },
		"Publisher":  func(d *Deps) { d.Publisher = nil },
		"Notifier":   func(d *Deps) { d.Notifier = nil },
		"Zones":      func(d *Deps) { d.Zones = nil },
		"Integrator": func(d *Deps) { d.Integrator = nil },
		"Rollovers":  func(d *Deps) { d.Rollovers = nil },
	}
	for name, drop := range tests {
		t.Run(name, func(t *testing.T) {
			deps := full()
			drop(&deps)
			if _, err := New(testConfig(), deps); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("New with a nil %s returned %v, want an error naming it", name, err)
			}
		})
	}

	// The two optional ones fall back rather than failing.
	if _, err := New(testConfig(), full()); err != nil {
		t.Errorf("New with a nil Clock and Logger = %v, want them defaulted", err)
	}
}

// Shutdown ordering. The Python got this backwards — it killed the network
// thread with the "offline" PUBLISH still queued and then sent a *clean*
// DISCONNECT, which per MQTT-3.14.4-3 tells the broker to discard the will, so
// both deliveries of "offline" could be lost from one shutdown.
//
// Here: STOPPING=1 first so the watchdog cannot trip while we drain, then our
// goroutines stop, and only then does mqttpub publish "offline" and pick its
// DISCONNECT reason code. The DLI state is flushed last, because the coalescing
// cadence otherwise leaves up to a minute of integration unwritten.
func TestShutdownOrdering(t *testing.T) {
	session := newHealthySession()
	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
	})

	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	waitFor(t, "the first cycle to publish", func() bool {
		_, ok := r.pub.last(r.stateTopic("sensor", ppfdObjectID))
		return ok
	})
	if r.notify.ready != 1 {
		t.Errorf("READY=1 sent %d times, want 1", r.notify.ready)
	}
	if r.notify.stopping != 0 {
		t.Error("STOPPING=1 was sent before the daemon was asked to stop")
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if r.notify.stopping != 1 {
		t.Errorf("STOPPING=1 sent %d times, want 1", r.notify.stopping)
	}
	if !r.pub.stopped {
		t.Fatal("the publisher was never shut down, so the retained offline was never published")
	}
	if r.pub.stopTopic != r.statusTopic() {
		t.Errorf("Shutdown was given %q, want the will topic %q", r.pub.stopTopic, r.statusTopic())
	}
	if r.acc.flushes() != 1 {
		t.Errorf("DLI flushed %d times, want exactly 1", r.acc.flushes())
	}
	if session.closeCount() != 1 {
		t.Errorf("the serial port was closed %d times, want 1; the poll goroutine owns it", session.closeCount())
	}
}

// The startup line has to state the outage timing in seconds, not only in
// cycles.
//
// OFFLINE_AFTER counts cycles and a silent cycle spends the whole cycle budget
// before it counts as failed, so OFFLINE_AFTER=3 at a 10 s cadence is "up to
// 99 s" and not 30. Nobody computes that from two numbers in a config dump: the
// operator waits thirty seconds for an outage that takes ninety-nine to appear
// and restarts a daemon that is working exactly as configured.
//
// --check-config says the same thing at more length and is pinned by its own
// golden test — but --check-config is run when someone is already suspicious.
// The journal line is what is there at 2am, and until this test existed it could
// be deleted with the whole suite staying green, which would silently make
// offlineAfterAtMost a dead method.
func TestTheStartupLineStatesTheOutageTimingInSeconds(t *testing.T) {
	r := newRig(t, withJournal(t))

	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	waitFor(t, "the startup line", func() bool { return r.journal.count("msg=started") == 1 })
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	line := r.journal.matching("msg=started")[0]
	// The value as well as the key: a line carrying the cycle count alone is the
	// number an operator gets wrong.
	for _, want := range []string{
		"offline_after_cycles=" + strconv.Itoa(r.cfg.OfflineAfter),
		"offline_after_at_most=" + r.offlineAfterAtMost().String(),
		"cycle_budget=" + r.cycleBudget.String(),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the startup line does not carry %q:\n  %s", want, line)
		}
	}
	if r.offlineAfterAtMost() == time.Duration(r.cfg.OfflineAfter)*r.cfg.PollInterval {
		t.Fatal("this rig cannot tell the two numbers apart, so the assertion above proves nothing")
	}
}

// Run must report a teardown that did not complete, because "the retained
// offline was not confirmed" is the one failure that leaves the bus lying.
func TestRunReportsTeardownFailures(t *testing.T) {
	boom := errors.New("could not persist the day")
	r := newRig(t, func(r *testRig) { r.acc.flushErr = boom })

	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()
	waitFor(t, "the first cycle", func() bool {
		_, ok := r.pub.last(r.stateTopic("sensor", ppfdObjectID))
		return ok
	})
	cancel()

	if err := <-runDone; !errors.Is(err, boom) {
		t.Errorf("Run = %v, want it to report the flush failure", err)
	}
}

// The timezone contract: apply, then echo the exact bytes the manager accepted,
// then hand the resulting zone to the integrator.
func TestTimezoneWriteIsAppliedEchoedAndFedToTheIntegrator(t *testing.T) {
	r := newRig(t)
	stateTopic := r.stateTopic(timeZoneComponent, timeZoneObjectID)
	const posix = "GMT0BST,M3.5.0/1,M10.5.0"

	r.applyTimezone(t.Context(), posix)

	if got := r.zones.writes(); len(got) != 1 || got[0] != posix {
		t.Errorf("Apply calls = %v, want [%q]", got, posix)
	}
	if got, _ := r.pub.last(stateTopic); got != posix {
		t.Errorf("echo = %q, want the exact accepted bytes %q", got, posix)
	}
	if got := r.acc.zonesSet(); len(got) != 1 || !strings.HasPrefix(got[0], posix+"|") {
		t.Errorf("integrator zones = %v, want the accepted zone", got)
	}
}

// A rejected write must re-echo the zone still in force. Echoing the received
// bytes instead would tell grow-app's reconciler its value took, and it would
// stop pushing — leaving the daemon on a zone nobody chose.
func TestARejectedTimezoneWriteReEchoesWhatIsInForce(t *testing.T) {
	r := newRig(t, func(r *testRig) { r.zones.state = "UTC0" })
	r.zones.rejectAll = true
	stateTopic := r.stateTopic(timeZoneComponent, timeZoneObjectID)

	r.applyTimezone(t.Context(), "this is not a POSIX TZ string")

	got, ok := r.pub.last(stateTopic)
	if !ok {
		t.Fatal("a rejected write published nothing; the reconciler would see its own value reflected")
	}
	if got != "UTC0" {
		t.Errorf("echo = %q, want the currently effective %q", got, "UTC0")
	}
}

// The subscription callback runs on autopaho's network goroutine and must never
// block it: an MQTT publish there would stall the reader and with it the
// keepalive that decides how quickly the will fires.
func TestTheTimezoneCallbackDoesNotBlockTheNetworkGoroutine(t *testing.T) {
	r := newRig(t)

	// Nothing is draining the channel, so every push past the first is dropped.
	// The callback still has to return promptly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 32 {
			r.onTimezoneCommand(r.timeZoneCommandTopic(), []byte("UTC0"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the subscription callback blocked")
	}

	// The one that was queued is applied when the goroutine gets to it.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tzDone := make(chan struct{})
	go func() { defer close(tzDone); r.runTimezone(ctx) }()
	waitFor(t, "the queued push to be applied", func() bool { return len(r.zones.writes()) > 0 })
	cancel()
	<-tzDone
}

// Run must subscribe to the command topic, and it must do so before Start so
// the SUBSCRIBE goes out ahead of the first birth sequence.
func TestRunSubscribesToTheTimezoneCommandTopic(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	waitFor(t, "the subscription", func() bool {
		r.pub.mu.Lock()
		defer r.pub.mu.Unlock()
		return r.pub.subs[r.timeZoneCommandTopic()] != nil
	})
	r.pub.deliver(t, r.timeZoneCommandTopic(), []byte("UTC0"))
	waitFor(t, "the pushed zone to be applied", func() bool { return len(r.zones.writes()) > 0 })

	cancel()
	<-runDone
}

// The derived values are published change-only: they are computed from samples
// already recorded at full rate, and the slow ones would otherwise contribute a
// flat line of identical points all day.
func TestDerivedValuesArePublishedOnChange(t *testing.T) {
	r := newRig(t)
	dliState := r.stateTopic("sensor", "dli")

	r.acc.set(dli.Reading{DLI: 1.5, PhotoperiodHours: 2, PeakPPFD: 900})
	r.publishDerived(t.Context(), r.acc.Snapshot(), true)
	r.publishDerived(t.Context(), r.acc.Snapshot(), true)
	if got := r.pub.payloads(dliState); len(got) != 1 || got[0] != "1.50" {
		t.Errorf("dli publishes = %v, want exactly [\"1.50\"]", got)
	}

	r.acc.set(dli.Reading{DLI: 1.75, PhotoperiodHours: 2, PeakPPFD: 900})
	r.publishDerived(t.Context(), r.acc.Snapshot(), true)
	if got := r.pub.payloads(dliState); len(got) != 2 || got[1] != "1.75" {
		t.Errorf("dli publishes = %v, want a second publish of 1.75", got)
	}
	// The unchanged siblings stayed quiet.
	if got := r.pub.payloads(r.stateTopic("sensor", "peak_ppfd")); len(got) != 1 {
		t.Errorf("peak_ppfd published %d times across three passes, want 1", len(got))
	}
}

// The rollover order is load-bearing: the completed day's whole total has to be
// the last value recorded against that date, so the reset publishes after it. A
// reversed order leaves the finished day's series ending at zero, which reads
// as a day with no light.
func TestRolloverPublishesTheFinalTotalBeforeTheReset(t *testing.T) {
	r := newRig(t)
	dliState := r.stateTopic("sensor", "dli")

	yesterdayState := r.stateTopic("sensor", "dli_yesterday")

	// The three steps dli emits, with the readings it emits them with: at
	// RolloverFinal "yesterday" still describes the day before the one closing,
	// and only the second step promotes.
	r.publishRollovers(t.Context(), []dli.Event{
		{Kind: dli.RolloverFinal, Day: "2026-07-27", Reading: dli.Reading{DLI: 31.25, Yesterday: 28}},
		{Kind: dli.RolloverYesterday, Day: "2026-07-27", Reading: dli.Reading{DLI: 31.25, Yesterday: 31.25}},
		{Kind: dli.RolloverReset, Day: "2026-07-28", Reading: dli.Reading{Yesterday: 31.25}},
	})

	if got := r.pub.payloads(dliState); len(got) != 2 || got[0] != "31.25" || got[1] != "0.00" {
		t.Errorf("dli publishes = %v, want the completed total then the reset", got)
	}
	if got, _ := r.pub.last(yesterdayState); got != "31.25" {
		t.Errorf("dli_yesterday ended at %q, want the completed day's total", got)
	}
	// The ordering that matters is across topics: the day's whole total has to
	// be recorded against its own date before anything else moves.
	finalAt := r.pub.indexOf(dliState, "31.25")
	promotedAt := r.pub.indexOf(yesterdayState, "31.25")
	resetAt := r.pub.indexOf(dliState, "0.00")
	if !(finalAt >= 0 && finalAt < promotedAt && promotedAt < resetAt) {
		t.Errorf("rollover order was final=%d promoted=%d reset=%d, want final < promoted < reset:\n  %s",
			finalAt, promotedAt, resetAt, formatMsgs(r.pub.observed()))
	}
}

// The sink exists because dli calls OnRollover with its own lock held and
// documents that the handler must not block; a publish there would hold the
// accumulator for a whole publish timeout against the timezone goroutine.
func TestRolloverSinkDrains(t *testing.T) {
	var s RolloverSink
	if got := s.Drain(); got != nil {
		t.Errorf("Drain on an empty sink = %v, want nil", got)
	}
	s.Handle(dli.Event{Kind: dli.RolloverFinal, Day: "a"})
	s.Handle(dli.Event{Kind: dli.RolloverReset, Day: "b"})

	got := s.Drain()
	if len(got) != 2 || got[0].Day != "a" || got[1].Day != "b" {
		t.Errorf("Drain = %+v, want the two events oldest first", got)
	}
	if again := s.Drain(); again != nil {
		t.Errorf("second Drain = %v, want nil; the sink must not replay", again)
	}
}

// The daemon must run with no state directory: systemd may grant none, and a
// sensor that refuses to publish is worse than one that forgets this morning.
func TestPersistenceDisabledIsNotAnError(t *testing.T) {
	cfg := testConfig()
	if cfg.PersistenceEnabled() {
		t.Fatal("testConfig unexpectedly has a state directory")
	}
	r := newRig(t, func(r *testRig) { r.cfg = cfg })
	if _, err := New(r.cfg, Deps{
		Dialer: r.dialer, Publisher: r.pub, Notifier: r.notify,
		Zones: r.zones, Integrator: r.acc, Rollovers: r.sink,
	}); err != nil {
		t.Errorf("New without a state directory = %v", err)
	}
}

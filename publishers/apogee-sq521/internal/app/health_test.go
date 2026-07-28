package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
)

// runHealth in the background, returning a stop func. The fake clock is what
// drives the ticks, so nothing here depends on wall time.
func (r *testRig) startHealth(t *testing.T) (context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.runHealth(ctx) }()
	return cancel, done
}

// Without a configured watchdog there is nothing to pet — but the status line
// still has to be refreshed.
//
// THE TRAP this closes: Notifier.Status used to be reachable only past the
// WatchdogInterval gate, so a unit with Type=notify and no WatchdogSec= sent
// READY=1, STOPPING=1 and no STATUS= ever. On the happy path this binary logs
// nothing after startup, so "empty journal and a blank status" was
// indistinguishable between healthy, wedged, and broker-partitioned — and the
// line saying the status had been disabled was at Debug, which the process
// logger discards.
func TestTheStatusLineIsRefreshedWithoutAWatchdog(t *testing.T) {
	r := newRig(t) // fakeNotifier.armed is false

	cancel, done := r.startHealth(t)
	defer func() { cancel(); <-done }()

	r.clock.awaitWaiters(t, 1)
	r.clock.Advance(statusRefresh)
	waitFor(t, "a status refresh", func() bool { return r.notify.lastStatus() != "" })

	if r.notify.pingCount() != 0 {
		t.Errorf("pings = %d with no watchdog configured, want 0", r.notify.pingCount())
	}
	if got := r.notify.lastStatus(); !strings.Contains(got, "PPFD") {
		t.Errorf("status line = %q, want the usual line", got)
	}
}

// While the poll goroutine is stamping, the watchdog is pet at half the
// configured interval and the status line is refreshed with it.
func TestWatchdogPingsWhileThePollLoopIsMakingProgress(t *testing.T) {
	r := newRig(t, func(r *testRig) {
		r.notify.interval, r.notify.armed = 60*time.Second, true
	})
	r.beat()

	cancel, done := r.startHealth(t)
	defer func() { cancel(); <-done }()

	for i := 1; i <= 3; i++ {
		r.clock.awaitWaiters(t, 1)
		r.beat() // the poll goroutine is alive
		r.clock.Advance(30 * time.Second)
		waitFor(t, "the watchdog ping", func() bool { return r.notify.pingCount() >= i })
	}
	if got := r.notify.lastStatus(); got == "" {
		t.Error("no status line was published alongside the pings")
	}
}

// The heartbeat means "the poll goroutine is scheduling", never "the sensor is
// answering". A sensor that has been unplugged must produce a retained
// "offline" and a steady retry — restarting the process every WatchdogSec would
// re-enumerate the USB device on a loop and lose the state that says what is
// wrong.
func TestADeadSensorDoesNotTripTheWatchdog(t *testing.T) {
	session := newHealthySession()
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
		r.notify.interval, r.notify.armed = 60*time.Second, true
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); r.runPoller(ctx) }()

	// cfg.OfflineAfter failed cycles take the device offline. The first runs as
	// soon as the session opens; the rest need a boundary each.
	for range r.cfg.OfflineAfter - 1 {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(r.cfg.PollInterval)
	}
	waitFor(t, "the device to be marked offline", func() bool {
		got, ok := r.pub.last(r.statusTopic())
		return ok && got == "offline"
	})

	// The poll goroutine is demonstrably alive — it just published an outage —
	// so the watchdog must still be pet.
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); r.runHealth(ctx) }()
	r.clock.awaitWaiters(t, 2) // the poll boundary plus the health tick
	r.clock.Advance(30 * time.Second)
	waitFor(t, "a watchdog ping despite the dead sensor", func() bool { return r.notify.pingCount() >= 1 })

	cancel()
	<-pollDone
	<-healthDone
}

// The failure the watchdog is actually good for: the goroutine itself stopped.
// Withholding the ping is what makes systemd send its WatchdogSignal (SIGABRT
// by default), and the Go runtime's stack dump into the journal is worth more
// than anything this daemon could log about a hang it does not understand.
func TestAStalledPollLoopStopsTheWatchdogPing(t *testing.T) {
	r := newRig(t, func(r *testRig) {
		r.notify.interval, r.notify.armed = 60*time.Second, true
	})
	r.beat()

	cancel, done := r.startHealth(t)
	defer func() { cancel(); <-done }()

	// One healthy tick first, so the test can tell "stopped pinging" from
	// "never started".
	r.clock.awaitWaiters(t, 1)
	r.clock.Advance(30 * time.Second)
	waitFor(t, "the first ping", func() bool { return r.notify.pingCount() == 1 })

	// Now the poll goroutine stops stamping. Past the tolerance, the pings
	// stop.
	tolerance := r.stalenessTolerance()
	r.clock.awaitWaiters(t, 1)
	r.clock.Advance(tolerance)
	// Two more ticks with no heartbeat.
	for range 2 {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(30 * time.Second)
	}
	// Give the goroutine a chance to have pinged if it were going to.
	waitFor(t, "the health loop to park again", func() bool { return len(r.clock.pending()) == 1 })
	if got := r.notify.pingCount(); got != 1 {
		t.Fatalf("pings = %d after the poll loop stalled, want it stuck at 1", got)
	}

	// And if the loop un-wedges, pinging resumes rather than having been
	// thrown away — systemd may well have a WatchdogSec generous enough for
	// that to matter.
	r.beat()
	r.clock.awaitWaiters(t, 1)
	r.clock.Advance(30 * time.Second)
	waitFor(t, "pings to resume", func() bool { return r.notify.pingCount() >= 2 })
}

// The tolerance has to cover every span the poll goroutine can legitimately
// spend not stamping, or the watchdog causes the restart loop it exists to
// prevent — and each restart re-enumerates the USB adapter.
//
// THE TRAP this closes is in the test rather than in the daemon. The per-term
// rows below are individually satisfied by almost any number: the original
// defective formula, 2×PollInterval + cycleBudget, is 53 s and clears every one
// of them (53 > 30, > 33, > 48). So for a while the *only* thing standing
// between this daemon and the 79 s-against-53 s restart loop was the arithmetic
// identity at the bottom — one line, backed by nothing, and the obvious thing
// for someone shrinking the formula to "fix" by pasting the new expression into
// it.
//
// The chain assertions are the independent backing. They are derived from where
// the beats actually are rather than from the formula, so they fail on a shrink
// whether or not the identity is touched, and the identity fails whether or not
// the chains are.
func TestStalenessToleranceCoversEveryLegitimateSpan(t *testing.T) {
	r := newRig(t)
	got := r.stalenessTolerance()

	// Necessary, nowhere near sufficient: no single wait may exceed the
	// tolerance on its own.
	spans := map[string]time.Duration{
		"the longest park between two serial open attempts": dialBackoffMax,
		"a whole poll cycle spent on a slow sensor":         r.cycleBudget,
		"a whole birth sequence against a slow broker":      r.birthBudget,
		"one poll boundary wait":                            r.cfg.PollInterval,
		"one serial transaction":                            transactionBudget(r.cfg.ReadTimeout),
		"one MQTT publish":                                  publishTimeout,
	}
	for what, span := range spans {
		if got <= span {
			t.Errorf("tolerance %s does not exceed %s (%s)", got, what, span)
		}
	}

	// The arrangements themselves. Each is a run of operations with no stamp
	// between them, read off the code rather than off stalenessTolerance:
	// runPoller stamps at the top of every dial attempt (poll.go, before Dial)
	// and cycle stamps at the top of every cycle, which is exactly why a dial
	// park and a measurement pass can never share an un-beaten span — and why
	// the tolerance is a sum of arrangements rather than one grand total anyone
	// can eyeball.
	//
	// The publishes inside these runs do stamp, but only when they publish:
	// announce skips a config the broker already holds and publishState skips an
	// unchanged payload, so a mute sensor's birth and a failing cycle's blanks
	// are both entirely no-ops on the second pass. Every span below therefore
	// assumes no stamp from them, which is the case that actually occurs.
	chains := map[string]time.Duration{
		// cycle stamps, measure runs every transaction without stamping between
		// them, and the sequence that follows waits for seqMu behind a
		// reconnection birth on autopaho's goroutine. Then the boundary wait,
		// and only then does the next cycle stamp.
		"a cycle's measurement pass, the wait for a reconnection birth's seqMu, and the boundary wait after it": r.cycleBudget + r.birthBudget + r.cfg.PollInterval,

		// The no-device path: the failed open's recordFailure waits for the same
		// seqMu, publishes blanks that are already blank, and then parks on the
		// backoff before the loop stamps again.
		"a failed open waiting out a reconnection birth and then parking on the dial backoff": r.birthBudget + dialBackoffMax,
	}
	for what, chain := range chains {
		if got <= chain {
			t.Errorf("tolerance %s does not exceed %s (%s); systemd would SIGABRT a working daemon on that arrangement",
				got, what, chain)
		}
	}

	// And the ceiling itself: the spans follow one another rather than
	// overlapping — a dial fails and parks on the backoff, the next open
	// succeeds, its session probes and births, and the first cycle then spends
	// its budget — so the tolerance is their sum, with a poll interval of slack
	// for a Pi that is also running something else.
	if want := dialBackoffMax + 2*r.cfg.PollInterval + r.cycleBudget + r.birthBudget; got != want {
		t.Errorf("tolerance = %s, want %s", got, want)
	}
}

// The empirical half, and the term the arrangement above is mostly made of: a
// reconnection births on autopaho's goroutine while holding seqMu, and the poll
// goroutine's next publish queues behind it without stamping.
//
// TestTheHeartbeatSurvivesAPortSessionRestartOnADeadSensor measures the poller
// against a slow sensor and a slow broker but never against a *contended* one —
// it reaches 43 s, which is under the defective 53 s tolerance and so cannot
// catch it. birthBudget is the largest term in the tolerance and this is the
// only test that makes the daemon actually spend it.
func TestTheToleranceCoversACycleQueuedBehindAReconnectionBirth(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	ctx := withPoller(t.Context())
	r.probeAndAnnounce(t, ctx, session)

	// A slow sensor and a slow broker, both legitimate: every transaction spends
	// its whole budget and every publish its whole timeout.
	budget := transactionBudget(r.cfg.ReadTimeout)
	session.onCall = func(string) { r.clock.Advance(budget) }
	r.pub.onPublish = func(context.Context, string) { r.clock.Advance(publishTimeout) }

	var mu sync.Mutex
	worst := time.Duration(0)
	r.clock.onAdvance = func(now time.Time) {
		gap := now.Sub(r.lastBeat())
		mu.Lock()
		worst = max(worst, gap)
		mu.Unlock()
	}

	// autopaho reports the link up. The birth runs on its goroutine, with Run's
	// context rather than the poller's, so none of its publishes stamp — and it
	// holds seqMu across the whole sequence.
	//
	// The two handshakes are what make the arrangement a fact rather than a race:
	// held says seqMu is genuinely taken before the cycle starts, and release
	// holds the birth's first publish back until the cycle has stamped, so every
	// instant either of them adds to the clock lands in the same un-beaten span.
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		r.sequence(func() {
			close(held)
			<-release
			r.birth(t.Context(), birthReconnected)
		})
	}()
	<-held

	// Released from inside the cycle, after cycle's own stamp. From here the poll
	// goroutine cannot stamp again until it has seqMu back.
	var once sync.Once
	session.onMeasure = func(string, int) { once.Do(func() { close(release) }) }

	r.cycle(ctx, session, &pollState{seen: map[string]int{}})
	<-done

	mu.Lock()
	got := worst
	mu.Unlock()
	tolerance := r.stalenessTolerance()
	t.Logf("worst un-beaten span %s against a tolerance of %s", got, tolerance)
	if got >= tolerance {
		t.Errorf("the poll goroutine went %s without stamping while a reconnection birth held seqMu, past the %s tolerance",
			got, tolerance)
	}
	// The arrangement really did happen. Without this the test would keep
	// passing if the contention silently stopped occurring, which is how it got
	// missed in the first place.
	if want := r.birthBudget; got < want {
		t.Errorf("worst span %s never reached the %s a contended birth costs; the arrangement did not occur", got, want)
	}
}

// The status line answers the questions an operator has at a terminal without
// reaching for the journal.
func TestStatusLine(t *testing.T) {
	r := newRig(t)

	if got := r.statusLine(); !strings.Contains(got, "PPFD n/a") {
		t.Errorf("status before the first reading = %q, want it to say PPFD is unavailable", got)
	}

	session := newHealthySession()
	session.script("4", step{err: errHeaderZero}) // a pre-3033 unit
	r.probeAndAnnounce(t, t.Context(), session)
	r.acc.set(dli.Reading{DLI: 18.4})
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})

	got := r.statusLine()
	for _, want := range []string{"PPFD 156.9 µmol/s/m²", "DLI 18.40 mol", "sensor ok", "mqtt up", "tilt n/a"} {
		if !strings.Contains(got, want) {
			t.Errorf("status line %q does not mention %q", got, want)
		}
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("status line %q contains a newline; sdnotify would parse it as another assignment", got)
	}
}

// HIGH 4. `systemctl status` must not affirmatively clear the component that is
// broken.
//
// The line used to derive everything from App.online and App.lastValues, both
// written only after a *successful* publish. So when the broker link dropped,
// nothing moved: the sensor was fine, the ladder never fired, nothing
// republished the availability — and the line kept saying "mqtt up" beside a
// reading that was hours old. Readings stop appearing in grow-app, the operator
// SSHes in, and the one line they read sends them to look at grow-app, the
// recorder and InfluxDB.
func TestStatusLineDistinguishesABrokerFaultFromASensorFault(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	st := &pollState{seen: map[string]int{}}
	r.cycle(t.Context(), session, st)

	healthy := r.statusLine()
	if !strings.Contains(healthy, "mqtt up") || !strings.Contains(healthy, "sensor ok") {
		t.Fatalf("status with everything working = %q", healthy)
	}

	// The broker goes away. The sensor is untouched and keeps answering, so
	// nothing in the failure ladder moves.
	r.pub.fail(context.DeadlineExceeded)
	for range 20 {
		r.clock.Advance(r.cfg.PollInterval)
		r.cycle(t.Context(), session, st)
	}

	brokerDown := r.statusLine()
	if brokerDown == healthy {
		t.Fatalf("the status line is identical with the broker up and down: %q", brokerDown)
	}
	if !strings.Contains(brokerDown, "mqtt DOWN") {
		t.Errorf("status with the broker unreachable = %q, want it to name the broken component", brokerDown)
	}
	if !strings.Contains(brokerDown, "sensor ok") {
		t.Errorf("status = %q; the sensor is answering and must not be blamed", brokerDown)
	}
	// The number it prints is stale, and it has to say so rather than reading as
	// current.
	if strings.Contains(brokerDown, "(0s old)") {
		t.Errorf("status = %q, want the reading's real age", brokerDown)
	}

	// And the other fault, so the two are genuinely distinguished rather than
	// merely differently worded.
	r.pub.fail(nil)
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	for range r.cfg.OfflineAfter {
		r.clock.Advance(r.cfg.PollInterval)
		r.cycle(t.Context(), session, st)
	}
	sensorDown := r.statusLine()
	if !strings.Contains(sensorDown, "sensor not answering") {
		t.Errorf("status with a dead sensor = %q, want it to name the sensor", sensorDown)
	}
	if !strings.Contains(sensorDown, "mqtt up") {
		t.Errorf("status = %q; the broker is reachable and must not be blamed", sensorDown)
	}
}

// HIGH 3. The watchdog must not kill the process on the dead-sensor path.
//
// Nothing used to stamp the heartbeat during probe or birth, so a port-session
// restart on a silent sensor left an un-beaten span of up to 79 s against a 53 s
// tolerance — measured, not hypothesised. systemd then SIGABRTs and restarts,
// and the restart re-enumerates the USB adapter: precisely the thrash runHealth
// exists to avoid, reached from the ordinary sensor-failure path.
//
// The assertion is made at every instant time actually passes, which is the
// only way to catch a span that opens and closes between two polls of a
// condition. Every serial transaction spends its whole budget and every publish
// its whole timeout — a slow sensor and a slow broker, both legitimate.
func TestTheHeartbeatSurvivesAPortSessionRestartOnADeadSensor(t *testing.T) {
	session := newHealthySession()
	session.identErr = errNoResponse
	session.verifyErr = errNoResponse
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}

	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
	})
	tolerance := r.stalenessTolerance()
	budget := transactionBudget(r.cfg.ReadTimeout)

	session.onCall = func(string) { r.clock.Advance(budget) }
	r.pub.onPublish = func(context.Context, string) { r.clock.Advance(publishTimeout) }

	var mu sync.Mutex
	worst := time.Duration(0)
	r.clock.onAdvance = func(now time.Time) {
		gap := now.Sub(r.lastBeat())
		mu.Lock()
		worst = max(worst, gap)
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	// Long enough for a reopen, which is settle, probe and birth with the
	// failure ladder already in force.
	for range 12 {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(r.cfg.PollInterval)
	}
	waitFor(t, "a second port session", func() bool { return r.dialer.dials() >= 2 })
	cancel()
	<-done

	mu.Lock()
	got := worst
	mu.Unlock()
	t.Logf("worst un-beaten span %s against a tolerance of %s", got, tolerance)
	if got >= tolerance {
		t.Errorf("the poll goroutine went %s without stamping, past the %s tolerance; systemd would SIGABRT a working daemon",
			got, tolerance)
	}
}

// The two places the stamp has to happen, pinned individually — the span test
// above is a property of the beats *and* the tolerance together, and either one
// alone would keep it passing while the other regressed.
//
// The bound is the budget of one operation, not the watchdog tolerance: the
// point is that the goroutine stamps *between* the units of work, so no single
// span is longer than the unit.
func TestTheProbeAndThePollersPublishesStampTheHeartbeat(t *testing.T) {
	tests := map[string]struct {
		bound time.Duration
		// setUp runs before the measurement starts, so a step's own cost does
		// not count against the step under test.
		setUp   func(r *testRig, ctx context.Context, s *fakeSession)
		measure func(r *testRig, ctx context.Context, s *fakeSession)
	}{
		// Three serial transactions, each spending its whole budget on a silent
		// sensor. Longer than a poll cycle, and reached from the ordinary
		// sensor-failure path via the reopen.
		"across a probe": {
			bound:   transactionBudget(testConfig().ReadTimeout),
			measure: func(r *testRig, ctx context.Context, s *fakeSession) { r.adopt(r.probe(ctx, s, &pollState{})) },
		},
		// A couple of dozen retained messages, each spending its whole timeout
		// against a slow broker.
		"across a birth on the poll goroutine": {
			bound:   publishTimeout,
			setUp:   func(r *testRig, ctx context.Context, s *fakeSession) { r.adopt(r.probe(ctx, s, &pollState{})) },
			measure: func(r *testRig, ctx context.Context, _ *fakeSession) { r.publishBirth(ctx, birthReconnected) },
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			session := newHealthySession()
			session.identErr = errNoResponse
			session.verifyErr = errNoResponse
			r := newRig(t)
			ctx := withPoller(t.Context())

			budget := transactionBudget(r.cfg.ReadTimeout)
			session.onCall = func(string) { r.clock.Advance(budget) }
			r.pub.onPublish = func(context.Context, string) { r.clock.Advance(publishTimeout) }

			if tc.setUp != nil {
				tc.setUp(r, ctx, session)
			}

			r.beat()
			worst := time.Duration(0)
			r.clock.onAdvance = func(now time.Time) { worst = max(worst, now.Sub(r.lastBeat())) }
			tc.measure(r, ctx, session)

			if worst > tc.bound {
				t.Errorf("the poll goroutine went %s without stamping, past the %s one operation may take",
					worst, tc.bound)
			}
		})
	}
}

// The other side of the same coin: a heartbeat stamped by a goroutine that is
// not the poll goroutine would feed the watchdog through exactly the wedge it
// exists to catch. A reconnection birth publishes a couple of dozen messages,
// and every publish stamps — but only when it really is the poller.
func TestAReconnectionBirthDoesNotStampTheHeartbeat(t *testing.T) {
	r := newRig(t)
	r.adopt(r.probe(t.Context(), newHealthySession(), &pollState{}))
	r.beat()

	before := r.lastBeat()
	r.clock.Advance(time.Minute)
	r.publishBirth(t.Context(), birthReconnected)

	if got := r.lastBeat(); !got.Equal(before) {
		t.Errorf("the heartbeat moved from %s to %s during a birth on autopaho's goroutine; a wedged poll loop would stay fed",
			before.UTC(), got.UTC())
	}
}

// M4. A notification socket that cannot be dialled cannot be dialled every
// tick either. Observed: 8 identical warnings in 40 s at WatchdogSec=10, i.e.
// ~5,760 a day at 30 — the exact defect every other recurring line in this
// daemon is guarded against.
func TestARepeatedWatchdogPingFailureIsLoggedOnce(t *testing.T) {
	r := newRig(t, withJournal(t), func(r *testRig) {
		r.notify.interval, r.notify.armed = 10*time.Second, true
		r.notify.pingErr = errors.New("dial unix /run/systemd/notify: connect: connection refused")
	})
	r.beat()

	cancel, done := r.startHealth(t)
	defer func() { cancel(); <-done }()

	for i := 1; i <= 8; i++ {
		r.clock.awaitWaiters(t, 1)
		r.beat()
		r.clock.Advance(5 * time.Second)
		waitFor(t, "the watchdog tick", func() bool { return r.notify.pingCount() >= i })
	}

	if n := r.journal.count("watchdog ping failed"); n != 1 {
		t.Errorf("%q logged %d times across eight ticks, want 1:\n%s", "watchdog ping failed", n, r.journal.dump())
	}

	// And it says so again when it recovers, or the operator has no way to tell
	// a fixed socket from a suppressed line.
	r.notify.setPingErr(nil)
	r.clock.awaitWaiters(t, 1)
	r.beat()
	r.clock.Advance(5 * time.Second)
	waitFor(t, "the recovery line", func() bool { return r.journal.count("watchdog ping recovered") == 1 })
}

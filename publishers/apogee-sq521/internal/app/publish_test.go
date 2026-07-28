package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A sensor that answers nothing gets reopened every reopenAfterSoftFailures
// cycles, and every reopen runs a fresh probe and a fresh birth. If the birth
// announced "online" unconditionally, the device card would alternate between
// online and offline for as long as the sensor stayed dead — which reads as an
// intermittent fault rather than a flat one, and is the harder thing to
// diagnose.
func TestAvailabilityDoesNotFlapAcrossReopensWhileTheSensorIsDead(t *testing.T) {
	session := newHealthySession()
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	session.identErr = errNoResponse
	session.verifyErr = errNoResponse

	r := newRig(t, func(r *testRig) {
		r.dialer = &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.runPoller(ctx) }()

	// Drive well past the first reopen: OfflineAfter cycles to go offline, then
	// reopenAfterSoftFailures to force the reopen, then a few more.
	for range 3 * (r.cfg.OfflineAfter + reopenAfterSoftFailures) {
		r.clock.awaitWaiters(t, 1)
		r.clock.Advance(r.cfg.PollInterval)
	}
	waitFor(t, "at least one reopen", func() bool { return r.dialer.dials() >= 2 })
	waitFor(t, "the device to be marked offline", func() bool {
		got, ok := r.pub.last(r.statusTopic())
		return ok && got == "offline"
	})

	cancel()
	<-done

	// The first birth runs before anything has failed, so one "online" is
	// honest. Everything after the ladder's verdict must stay "offline" —
	// including the births that each reopen triggers, which is the flap.
	got := r.pub.payloads(r.statusTopic())
	if len(got) < 2 {
		t.Fatalf("availability publishes = %v, want the initial online followed by an outage", got)
	}
	if got[0] != "online" {
		t.Errorf("the first availability publish was %q, want online", got[0])
	}
	for i, payload := range got[1:] {
		if payload == "online" {
			t.Fatalf("availability publish %d of %v claimed online again while the sensor was answering nothing",
				i+1, got)
		}
	}
}

// The pre-probe availability correction must not be reported as an outage.
//
// The publish itself is deliberate and stays: a previous run's "online" left
// retained on the broker has grow-app render its last reading as live, and a PAR
// sensor sitting at a constant value looks exactly like night rather than like a
// dead daemon. But it went out through the same line M6 built for the failure
// ladder, so a perfectly healthy unit wrote
//
//	WARN device is unavailable; readings are invalidated until it answers again
//	     reason="" failed_attempts=0 error=<nil>
//
// on every single start. Two harms, and the second is the worse one: an operator
// who sees that line on every restart stops reading the line M6 exists to make
// actionable, and the content is not even stable — a dial failure that beats the
// CONNACK fills the same fields in, so the same start emits two different lines
// depending on which lost the race.
func TestACleanStartDoesNotReportAnOutage(t *testing.T) {
	r := newRig(t, withJournal(t))

	// mqtt comes up before the probe has run, which is the ordinary startup
	// order: Publisher.Start is non-blocking and the serial open lives behind it.
	r.publishBirth(t.Context(), birthReconnected)

	if got, ok := r.pub.last(r.statusTopic()); !ok || got != "offline" {
		t.Fatalf("status = %q (published=%v), want the retained offline that corrects a previous run",
			got, ok)
	}
	if n := r.journal.count("device is unavailable"); n != 0 {
		t.Errorf("the outage line fired %d times on a clean start:\n%s", n, r.journal.dump())
	}
	if n := r.journal.count("level=WARN"); n != 0 {
		t.Errorf("a healthy start wrote %d warnings; that is how an operator learns to skip them:\n%s",
			n, r.journal.dump())
	}
}

// A retraction that could not be published is not recorded as done, because the
// config really may still be retained on the broker — recording it would leave
// it there forever with nothing willing to try again.
func TestAFailedRetractionIsRetried(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	tiltConfig := r.discoveryTopic("sensor", "tilt")

	// Retire tilt while the broker is unreachable.
	r.retire("tilt")
	r.pub.fail(errors.New("connection with the MQTT server is currently down"))
	r.publishBirth(t.Context(), birthLocal)

	if payload, recorded := r.retainedFor("tilt"); recorded && payload == "" {
		t.Fatal("a retraction that failed to publish was recorded as done; the retained config would outlive the daemon")
	}

	// When the link comes back, the next birth clears it.
	r.pub.fail(nil)
	r.pub.reset()
	r.publishBirth(t.Context(), birthLocal)

	got, ok := r.pub.last(tiltConfig)
	if !ok || got != "" {
		t.Errorf("retraction on retry = %q (published=%v), want the empty payload", got, ok)
	}
	if payload, recorded := r.retainedFor("tilt"); !recorded || payload != "" {
		t.Errorf("after a successful retraction, retained[tilt] = %q (recorded=%v), want the empty payload",
			payload, recorded)
	}
}

// A config that failed to publish must not be recorded as retained, or the next
// birth would skip it as already announced.
func TestAFailedAnnouncementIsNotRecorded(t *testing.T) {
	r := newRig(t)
	r.adopt(r.probe(t.Context(), newHealthySession(), &pollState{}))

	r.pub.fail(errors.New("connection with the MQTT server is currently down"))
	r.publishBirth(t.Context(), birthLocal)

	r.mu.Lock()
	tracked := len(r.retained)
	r.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d configs recorded as retained despite every publish failing", tracked)
	}

	// And the next birth, with the link back, really does announce them.
	r.pub.fail(nil)
	r.publishBirth(t.Context(), birthLocal)
	if _, ok := r.pub.last(r.discoveryTopic("sensor", ppfdObjectID)); !ok {
		t.Error("the birth after the outage did not announce ppfd; the failed publish was treated as done")
	}
}

// retainedFor reports what the App believes the broker holds for one entity.
func (r *testRig) retainedFor(objectID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload, ok := r.retained[objectID]
	return payload, ok
}

// CONSTRAINT 4, across a process boundary — which is the case that actually
// happens.
//
// A retraction diffed against what *this* process published is empty on every
// restart: systemd, a deploy, a reboot, a sensor swap. The fresh process has
// announced nothing, so it has nothing to retract, and the config the previous
// run left retained on the broker stands forever. grow-app then registers a
// permanently valueless entity — verbatim the defect constraints 4 and 5 exist
// to prevent, arrived at by the most common path there is.
//
// Two Apps sharing one publisher is one broker seeing two processes.
func TestAConfigRetainedByAPreviousProcessIsRetracted(t *testing.T) {
	// Process 1: a 3033+ unit. tilt is supported, so it is announced.
	first := newRig(t)
	first.probeAndAnnounce(t, t.Context(), newHealthySession())
	tiltConfig := first.discoveryTopic("sensor", "tilt")
	if got, ok := first.pub.last(tiltConfig); !ok || got == "" {
		t.Fatal("process 1 never announced tilt, so there is nothing for process 2 to clean up")
	}

	// Process 2: the same broker, the sensor swapped for a pre-3033 unit. It
	// has published nothing at all and remembers nothing.
	broker := first.pub
	second := newRig(t, func(r *testRig) { r.pub = broker })
	broker.reset()
	swapped := newHealthySession()
	swapped.script("4", step{err: errHeaderZero})
	second.probeAndAnnounce(t, t.Context(), swapped)

	got, ok := broker.last(tiltConfig)
	if !ok {
		t.Fatalf("process 2 said nothing about tilt; the previous run's retained config lives on the broker forever:\n  %s",
			formatMsgs(broker.observed()))
	}
	if got != "" {
		t.Errorf("process 2 published %q to %s, want the empty payload that clears it", got, tiltConfig)
	}
	if got, ok := broker.last(second.stateTopic("sensor", "tilt")); !ok || got != "" {
		t.Errorf("tilt's state topic = %q (published=%v), want it cleared too", got, ok)
	}
}

// The other half: asserting ineligibility must not turn into asserting it
// forever. The retraction is recorded exactly as an announcement is, so a mute
// sensor's reopen loop does not republish it every session.
func TestARetractionIsAssertedOnceNotEveryBirth(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	session.script("4", step{err: errHeaderZero})
	st := &pollState{}
	r.probeAndAnnounceWith(t, t.Context(), session, st)

	r.pub.reset()
	for range 5 {
		r.probeAndAnnounceWith(t, t.Context(), session, st)
	}
	if got := r.pub.payloads(r.discoveryTopic("sensor", "tilt")); len(got) != 0 {
		t.Errorf("tilt's retraction was republished %d times across five further births (%v)", len(got), got)
	}
}

// HIGH 5. A mute-but-enumerated sensor must not flood the broker.
//
// Reopen every reopenAfterSoftFailures cycles, re-probe, re-birth: at
// POLL_SECONDS=10 a port session is 30-100 s, so an unconditional birth is
// 11k-38k retained discovery publishes a day, indefinitely, for one unplugged
// sensor wire. And a retained PUBLISH is forwarded to every current subscriber
// whether or not the payload changed, so the churn is real on grow-app's side
// rather than merely wasteful on ours.
func TestAMuteSensorDoesNotRepublishItsConfigsEverySession(t *testing.T) {
	session := newHealthySession()
	session.identErr = errNoResponse
	session.verifyErr = errNoResponse
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	r := newRig(t, func(r *testRig) {
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
	configs := 0
	for _, m := range r.pub.observed() {
		if strings.Contains(m.topic, "/_discovery/") {
			configs++
		}
	}
	// One assertion per entity for the whole run, whatever the session count:
	// the eligible ones announced once, the ineligible ones retracted once.
	if want := len(r.table) + len(r.aux); configs > want {
		t.Errorf("%d discovery publishes across %d port sessions, want at most %d — one per entity, ever:\n  %s",
			configs, sessions, want, formatMsgs(r.pub.observed()))
	}
}

// M2. A reconnection births on autopaho's goroutine and must not interleave
// with the poller's own publishes.
//
// Two harms, both invisible without the serialisation: a retained value can
// land ahead of the discovery config that resolves it, and the publish-then-
// record pairs can leave "online" retained on the broker while App.online
// records false — which setAvailability, publishing only on change, never
// corrects. Bounded to seconds by the fact that a failing sensor re-births
// every few cycles, but a stale retained "online" is this project's headline
// failure mode.
func TestAReconnectionBirthCannotInterleaveWithACycle(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	r.pub.reset()

	// Every publish takes real time, which is what opens a window the two
	// goroutines could interleave in. A birth is a couple of dozen messages and
	// a cycle a handful, so without the exclusion they overlap for certain
	// rather than probably.
	r.pub.mu.Lock()
	r.pub.delay = time.Millisecond
	r.pub.mu.Unlock()

	// autopaho reports the link up while the cycle is publishing, on its own
	// goroutine and with Run's context — the one the poller has NOT marked,
	// which is how each message is attributed below.
	var once sync.Once
	reconnected := make(chan struct{})
	r.pub.onPublish = func(context.Context, string) {
		once.Do(func() {
			go func() { defer close(reconnected); r.publishBirth(t.Context(), birthReconnected) }()
		})
	}

	r.cycle(withPoller(t.Context()), session, &pollState{seen: map[string]int{}})
	<-reconnected

	// Non-interleaving, stated on the trace: each goroutine's publishes form one
	// unbroken run, so neither sequence is spliced into the other.
	msgs := r.pub.observed()
	var poller, birth []int
	for i, m := range msgs {
		if m.fromPoller {
			poller = append(poller, i)
		} else {
			birth = append(birth, i)
		}
	}
	if len(poller) == 0 || len(birth) == 0 {
		t.Fatalf("the two sequences did not both publish (poller=%d birth=%d):\n  %s",
			len(poller), len(birth), formatMsgs(msgs))
	}
	if !contiguous(poller) {
		t.Errorf("the cycle's publishes at %v were broken up by the reconnection birth:\n  %s",
			poller, formatMsgs(msgs))
	}
	if !contiguous(birth) {
		t.Errorf("the reconnection birth's publishes at %v were broken up by the cycle:\n  %s",
			birth, formatMsgs(msgs))
	}

	// And the retained availability agrees with what the App recorded, which is
	// the divergence that never self-corrects.
	retainedStatus, ok := r.pub.last(r.statusTopic())
	if !ok {
		t.Fatal("nothing was published to the status topic")
	}
	r.mu.Lock()
	recorded := r.online
	r.mu.Unlock()
	if (retainedStatus == "online") != recorded {
		t.Errorf("the broker retains %q but App.online is %v; setAvailability only publishes on change, so nothing corrects this",
			retainedStatus, recorded)
	}
}

// contiguous reports whether the indices form one unbroken run.
func contiguous(idx []int) bool {
	for i := 1; i < len(idx); i++ {
		if idx[i] != idx[i-1]+1 {
			return false
		}
	}
	return true
}

// A polled value is republished every cycle even when it has not moved: it is a
// measurement, and grow-history-recorder builds a time series out of it. A PAR
// sensor sitting at a constant value overnight is data, not a repeat worth
// suppressing.
func TestUnchangedReadingsAreStillRepublished(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)

	r.pub.reset()
	st := &pollState{seen: map[string]int{}}
	for range 3 {
		r.cycle(t.Context(), session, st)
	}

	got := r.pub.payloads(r.stateTopic("sensor", ppfdObjectID))
	if len(got) != 3 {
		t.Errorf("ppfd published %d times across three identical cycles, want 3", len(got))
	}
	for _, payload := range got {
		if payload != "156.9" {
			t.Errorf("ppfd payload = %q, want 156.9", payload)
		}
	}
}

// Everything is republished on a reconnection, because mqttpub creates its
// session with CleanStart and a broker that restarted has forgotten every
// retained message we ever sent. A daemon that announced only once would be
// invisible until it happened to be restarted itself.
//
// This is the case the birth's idempotency must not break: the skip is keyed on
// what this process believes the broker holds, and a connection-up is exactly
// the moment that belief stops being evidence.
func TestEverythingIsRepublishedOnAReconnection(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)

	r.pub.reset()
	r.publishBirth(t.Context(), birthReconnected)

	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		if _, ok := r.pub.last(r.stateTopic("sensor", objectID)); !ok {
			t.Errorf("%s state was not republished after the reconnection", objectID)
		}
		if _, ok := r.pub.last(r.discoveryTopic("sensor", objectID)); !ok {
			t.Errorf("%s discovery was not republished after the reconnection", objectID)
		}
	}
	for _, objectID := range []string{ppfdObjectID, "detector_mv", "tilt"} {
		if _, ok := r.pub.last(r.discoveryTopic("sensor", objectID)); !ok {
			t.Errorf("%s discovery was not republished after the reconnection", objectID)
		}
	}
	for _, objectID := range []string{timeZoneObjectID} {
		if _, ok := r.pub.last(r.discoveryTopic(timeZoneComponent, objectID)); !ok {
			t.Errorf("%s discovery was not republished after the reconnection", objectID)
		}
	}
	if got, ok := r.pub.last(r.statusTopic()); !ok || got != "online" {
		t.Errorf("availability after the reconnection = %q (published=%v), want online", got, ok)
	}
	if _, ok := r.pub.last(r.stateTopic("sensor", "dli")); !ok {
		t.Error("the derived values were not republished after the reconnection")
	}
}

// The retracted entity's state topic is cleared too. Without a config nothing
// resolves it, so the value is inert either way — but a future config on the
// same topic would silently inherit the stale reading.
func TestRetractionAlsoClearsTheStateTopic(t *testing.T) {
	r := newRig(t)
	session := newHealthySession()
	r.probeAndAnnounce(t, t.Context(), session)
	r.cycle(t.Context(), session, &pollState{seen: map[string]int{}})

	tiltState := r.stateTopic("sensor", "tilt")
	if got, _ := r.pub.last(tiltState); got != "1.20" {
		t.Fatalf("tilt state = %q, want a reading to invalidate", got)
	}

	r.pub.reset()
	r.retire("tilt")
	r.publishBirth(t.Context(), birthLocal)

	if got, ok := r.pub.last(tiltState); !ok || got != "" {
		t.Errorf("tilt state after retraction = %q (published=%v), want the empty payload", got, ok)
	}
}

// The birth must survive being run twice concurrently — the poller starts one
// when the probe completes and a reconnection can start another at the same
// instant — without interleaving two sequences.
func TestConcurrentBirthsAreSerialised(t *testing.T) {
	r := newRig(t)
	r.adopt(r.probe(t.Context(), newHealthySession(), &pollState{}))

	done := make(chan struct{}, 4)
	for range 4 {
		// Reconnections, so each one really republishes the whole set rather
		// than skipping everything as unchanged.
		go func() { r.publishBirth(t.Context(), birthReconnected); done <- struct{}{} }()
	}
	for range 4 {
		<-done
	}

	// Whatever the interleaving, every sequence ends with the availability, so
	// the last message of each run of publishes is the status. Checking the
	// simpler invariant: the final message overall is the availability or a
	// value published after it, never a discovery config.
	msgs := r.pub.observed()
	if len(msgs) == 0 {
		t.Fatal("nothing was published")
	}
	statusAt := -1
	for i, m := range msgs {
		if m.topic == r.statusTopic() {
			statusAt = i
		}
	}
	for i := statusAt + 1; i < len(msgs); i++ {
		if msgs[i].topic == r.discoveryTopic("sensor", ppfdObjectID) {
			t.Fatalf("a discovery config was published after the last availability; two births interleaved:\n  %s",
				formatMsgs(msgs))
		}
	}
}

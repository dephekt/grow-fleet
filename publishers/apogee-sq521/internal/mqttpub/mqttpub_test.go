package mqttpub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho/session"
)

// Topics mirror what entities.Topics would produce for this node, so the tests
// read like the daemon does.
const (
	testNodeID     = "quantum-sensor"
	baseTopic      = "grow/test/" + testNodeID
	statusTopic    = baseTopic + "/status"
	stateTopic     = baseTopic + "/sensor/ppfd/state"
	discoveryTopic = "grow/test/_discovery/sensor/" + testNodeID + "/ppfd/config"
	wildcard       = "grow/test/#"
)

// newPublisher builds a Publisher pointed at addr. Timeouts are shortened from
// the production defaults so the suite runs in seconds rather than minutes;
// nothing else is changed.
func newPublisher(t *testing.T, addr string, mutate func(*Config)) *Publisher {
	t.Helper()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}

	cfg := Config{
		Host:              host,
		Port:              port,
		NodeID:            testNodeID,
		StatusTopic:       statusTopic,
		ShutdownTimeout:   2 * time.Second,
		ConnectRetryDelay: 100 * time.Millisecond,
		ConnectTimeout:    2 * time.Second,
		Logger:            testLogger(t),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// birth registers the daemon's real connection-up sequence: discovery first,
// then the retained "online".
func birth(t *testing.T, p *Publisher) {
	t.Helper()
	p.OnConnectionUp(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.PublishRetained(ctx, discoveryTopic, []byte(`{"name":"PPFD"}`)); err != nil {
			t.Logf("discovery publish: %v", err)
		}
		if err := p.PublishRetained(ctx, statusTopic, []byte(PayloadOnline)); err != nil {
			t.Logf("birth publish: %v", err)
		}
	})
}

func mustStart(t *testing.T, p *Publisher) {
	t.Helper()
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func mustConnect(t *testing.T, p *Publisher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.AwaitConnection(ctx); err != nil {
		t.Fatalf("AwaitConnection: %v", err)
	}
}

// waitUntil polls cond, failing the test if it does not hold within timeout.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// sigtermContext returns an already-cancelled context, which is what Shutdown
// is handed in production: main's context is cancelled by the signal handler
// before the deferred Shutdown runs. Passing a live context here would make
// these tests pass even if Shutdown dropped context.WithoutCancel.
func sigtermContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestShutdownPublishesOfflineOnceWithReachableBroker is the happy path: the
// retained "offline" is confirmed by a PUBACK, so the DISCONNECT carries reason
// code 0x00 and the broker discards the will. Exactly one "offline" must reach
// the bus — a second one would mean we asked for the will unnecessarily.
func TestShutdownPublishesOfflineOnceWithReachableBroker(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)

	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	b.waitFor("retained offline", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOffline) >= 1
	})

	// Give a suppressed will time to show up if it was not actually suppressed;
	// it would follow the DISCONNECT within microseconds. Two "offline"s means
	// the will fired as well, i.e. the reason code was 0x04 when it should have
	// been 0x00.
	assertRetainedOnce(t, b.settle(300*time.Millisecond), statusTopic, PayloadOffline)

	// The status topic's entire history, in order: one birth, one teardown, and
	// nothing after the teardown.
	if got, want := b.payloads(statusTopic), []string{PayloadOnline, PayloadOffline}; !slices.Equal(got, want) {
		t.Errorf("status topic saw %v, want %v", got, want)
	}

	// What the broker actually stored is what grow-app will read on startup.
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q", got, ok, PayloadOffline)
	}
}

// TestShutdownFiresWillWhenOfflinePublishFails is the regression test for the
// defect this package exists to fix.
//
// The broker drops the "offline" PUBLISH without acknowledging it, so the
// publish times out. The connection itself stays healthy, which is what makes
// this a test of the reason code rather than of TCP: the DISCONNECT is written
// and read normally, and the only reason a retained "offline" reaches the bus
// is that we asked for reason code 0x04 instead of 0x00.
//
// Under the Python's teardown the status would be left saying "online" here.
func TestShutdownFiresWillWhenOfflinePublishFails(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr, &rejectHook{topic: statusTopic, payload: PayloadOffline})
	b.watch(wildcard)

	p := newPublisher(t, addr, func(c *Config) {
		// The publish gets 3/4 of this before it is abandoned.
		c.ShutdownTimeout = 800 * time.Millisecond
	})
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)

	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOnline {
		t.Fatalf("retained status before shutdown = %q (present=%v), want %q", got, ok, PayloadOnline)
	}

	err := p.Shutdown(sigtermContext(), statusTopic)
	if err == nil {
		t.Fatal("Shutdown returned nil, want an error reporting the unconfirmed offline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want one wrapping context.DeadlineExceeded", err)
	}

	// The client never got a PUBACK, but the broker must still end up holding a
	// retained "offline" — published by the will.
	b.waitFor("will-published offline", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOffline) >= 1
	})
	assertRetainedOnce(t, b.observed(), statusTopic, PayloadOffline)
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q; the will did not fire", got, ok, PayloadOffline)
	}
}

// TestShutdownRefusesAStatusTopicTheWillDoesNotCover is the parameter-shaped
// route back into the original defect.
//
// The will is registered on Config.StatusTopic at CONNECT and cannot be
// re-pointed at shutdown. If Shutdown obeyed a different topic, the "offline"
// would be PUBACKed, offlineConfirmed would go true, the DISCONNECT would carry
// 0x00 — and the broker would discard the will covering the *real* status
// topic, leaving it retained as "online" for a process that has exited.
//
// So the mismatch is refused, nothing is published, and the will does the work.
func TestShutdownRefusesAStatusTopicTheWillDoesNotCover(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)
	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	wrong := baseTopic + "/availability"
	err := p.Shutdown(sigtermContext(), wrong)

	// The broker-side outcome first, because that is the defect itself: obey the
	// argument and the retained status is still "online" here.
	msgs := b.settle(300 * time.Millisecond)
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q — a confirmed \"offline\" on some other topic suppressed the will that covers this one",
			got, ok, PayloadOffline)
	}
	// An "offline" on the wrong topic is a record nothing reads, and publishing
	// it is exactly what would have set offlineConfirmed and discarded the will.
	if got := count(msgs, wrong, PayloadOffline); got != 0 {
		t.Errorf("observed %d %q publishes on the mismatched topic %s, want 0: %s",
			got, PayloadOffline, wrong, format(msgs))
	}
	if !errors.Is(err, ErrStatusTopicMismatch) {
		t.Errorf("Shutdown with a mismatched status topic = %v, want %v", err, ErrStatusTopicMismatch)
	}
}

// TestShutdownAcceptsTheWillTopicOrNothing: the argument is a check, so the two
// things that pass the check must both still work.
func TestShutdownAcceptsTheWillTopicOrNothing(t *testing.T) {
	for _, tt := range []struct {
		name  string
		topic string
	}{
		{"the will topic", statusTopic},
		{"empty, meaning the will topic", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			addr := freeAddr(t)
			b := startBroker(t, addr)
			b.watch(wildcard)

			p := newPublisher(t, addr, nil)
			mustStart(t, p)
			mustConnect(t, p)

			if err := p.Shutdown(sigtermContext(), tt.topic); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			assertRetainedOnce(t, b.settle(300*time.Millisecond), statusTopic, PayloadOffline)
			if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
				t.Errorf("retained status = %q (present=%v), want %q", got, ok, PayloadOffline)
			}
		})
	}
}

// TestBirthReAnnouncesOnEveryConnection pins the reconnection half of the birth
// contract: the whole sequence runs again, in the same order, after the link
// has been re-established.
//
// The session is created with CleanStart and SessionExpiryInterval 0, so a
// broker that restarted has forgotten everything: re-announcing on every
// connection is the only thing that makes the daemon self-healing. grow-app
// registers the device from the discovery config, so an "online" arriving
// before it is attributed to a device it has never heard of and dropped.
//
// The ordering *within* one callback is the caller's own doing — this test's
// callback publishes discovery then online, and it would still pass if the
// package ran callbacks in any order. The package-level guarantees (callbacks
// in registration order, subscriptions issued before any callback) are pinned
// by TestConnectionUpSubscribesBeforeCallbacksAndRunsThemInOrder.
func TestBirthReAnnouncesOnEveryConnection(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)

	b.waitFor("first birth", 10*time.Second, func(msgs []message) bool {
		return len(birthSequence(msgs)) >= 2
	})

	// Kick the client off from the broker side, which is as close to a real
	// broker restart as this harness gets.
	b.evict(ClientID(testNodeID))

	b.waitFor("second birth after reconnect", 10*time.Second, func(msgs []message) bool {
		return len(birthSequence(msgs)) >= 4
	})

	got := birthSequence(b.observed())
	want := []string{"discovery", "online", "discovery", "online"}
	if len(got) < len(want) {
		t.Fatalf("birth sequence = %v, want at least %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("birth sequence = %v, want prefix %v", got, want)
		}
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestConnectionUpSubscribesBeforeCallbacksAndRunsThemInOrder pins the two
// ordering guarantees the package makes about the connection-up sequence, as
// opposed to the ordering a caller's own callback happens to produce.
//
// Subscriptions go first because a callback that publishes a state whose
// command topic is not yet subscribed would miss a reply sent immediately
// after — the daemon would announce itself and then be deaf for one round trip.
// Callbacks then run in registration order, which is what lets a caller put
// discovery ahead of the birth "online".
//
// Both are observed from the broker: a SUBSCRIBE and a QoS 1 PUBLISH are each
// round trips, so the order the broker processed them in is the order the
// Publisher issued them in.
func TestConnectionUpSubscribesBeforeCallbacksAndRunsThemInOrder(t *testing.T) {
	addr := freeAddr(t)
	events := &eventHook{clientID: ClientID(testNodeID)}
	startBroker(t, addr, events)

	p := newPublisher(t, addr, nil)

	cmdTopic := baseTopic + "/sensor/ppfd/command"
	if err := p.Subscribe(context.Background(), cmdTopic, func(string, []byte) {}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	names := []string{"first", "second", "third"}
	var mu sync.Mutex
	var ran []string
	for _, name := range names {
		p.OnConnectionUp(func() {
			mu.Lock()
			ran = append(ran, name)
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.PublishRetained(ctx, baseTopic+"/callback/"+name, []byte("x")); err != nil {
				t.Errorf("callback %s publish: %v", name, err)
			}
		})
	}

	mustStart(t, p)
	mustConnect(t, p)

	want := []string{
		"subscribe " + cmdTopic,
		"publish " + baseTopic + "/callback/first",
		"publish " + baseTopic + "/callback/second",
		"publish " + baseTopic + "/callback/third",
	}
	waitUntil(t, "the connection-up sequence to finish", 10*time.Second, func() bool {
		return len(events.recorded()) >= len(want)
	})

	if got := events.recorded(); !slices.Equal(got[:len(want)], want) {
		t.Errorf("broker processed %v, want it to start with %v", got, want)
	}

	mu.Lock()
	got := slices.Clone(ran)
	mu.Unlock()
	if !slices.Equal(got, names) {
		t.Errorf("callbacks ran %v, want %v (registration order)", got, names)
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// birthSequence reduces a recording to just the discovery/online events, in
// order, so the assertion is about ordering rather than about the retained
// "offline" the broker publishes when it evicts the client.
func birthSequence(msgs []message) []string {
	var seq []string
	for _, m := range msgs {
		switch {
		case m.topic == discoveryTopic:
			seq = append(seq, "discovery")
		case m.topic == statusTopic && m.payload == PayloadOnline:
			seq = append(seq, "online")
		}
	}
	return seq
}

// TestPublishWhileBrokerDownFailsAndDoesNotQueue pins the decision not to use
// PublishViaQueue. A queued reading would be delivered on reconnection, landing
// on a retained state topic where it is indistinguishable from a fresh one —
// grow-app would show a reading from the middle of the outage as live.
func TestPublishWhileBrokerDownFailsAndDoesNotQueue(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)
	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	b.stop()

	// Publish the same value repeatedly until the manager refuses it outright.
	// The early attempts matter as much as the last one: an attempt made in the
	// instant before the drop is noticed is accepted into paho's session state
	// and only fails when its context expires. That one must not be resent when
	// the link returns either, which is why every attempt carries the same
	// sentinel payload. (TestAbandonedPublishIsNotReplayedOnReconnect arranges
	// that case deliberately rather than hoping to catch it here.)
	const outageValue = "1234.5"
	waitUntil(t, "a publish to be refused rather than queued", 10*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		return connectionUnavailable(p.PublishRetained(ctx, stateTopic, []byte(outageValue)))
	})

	// A fresh broker on the same address: its retained store starts empty, so
	// anything that shows up now came from the client.
	b2 := startBroker(t, addr)
	b2.watch(wildcard)
	mustConnect(t, p)
	b2.waitFor("reconnect birth", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	for _, m := range b2.settle(300 * time.Millisecond) {
		if m.payload == outageValue {
			t.Errorf("reading published during the outage was replayed on reconnect: %s", m)
		}
	}
	if got, ok := b2.retained(stateTopic); ok {
		t.Errorf("retained state = %q, want nothing (the outage publish must not be queued)", got)
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestAbandonedPublishIsNotReplayedOnReconnect pins SessionExpiryInterval 0,
// which is the second half of the no-stale-readings rule and the half that
// declining PublishViaQueue does not cover.
//
// The setup is the one that makes the difference visible: a QoS 1 reading is
// accepted by the broker and then never acknowledged, so it stays in paho's
// session state, and *then* the connection drops. With a session that outlives
// the connection, paho retransmits it on reconnection — a reading from the
// middle of an outage, landing retained on a state topic where grow-app cannot
// tell it from a fresh one. With SessionExpiryInterval 0 the store is cleaned
// when the connection is lost, so the reading is dropped and the caller is told.
//
// The reconnection is to the *same* broker on purpose. A restarted broker
// answers CONNACK with SessionPresent false, which makes paho discard the store
// no matter how the session was configured — so a fresh-broker test would pass
// either way and prove nothing.
func TestAbandonedPublishIsNotReplayedOnReconnect(t *testing.T) {
	const outageValue = "1234.5"

	addr := freeAddr(t)
	reject := &rejectHook{topic: stateTopic, payload: outageValue}
	b := startBroker(t, addr, reject)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)
	mustStart(t, p)
	mustConnect(t, p)
	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	// The publish reaches the broker and is dropped there without a PUBACK, so
	// it is in the session store and the call is still waiting.
	pubErr := make(chan error, 1)
	go func() {
		// A deadline short enough to keep the test quick. Without one the call
		// sits for paho's 10s PacketTimeout, because losing the connection does
		// not wake a publish that is waiting on a PUBACK — which is itself worth
		// knowing: the reading is gone either way, and only the caller's own
		// deadline says when.
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		pubErr <- p.PublishRetained(ctx, stateTopic, []byte(outageValue))
	}()
	waitUntil(t, "the broker to swallow the reading", 10*time.Second, func() bool {
		return reject.rejected() >= 1
	})

	// From here on a retransmission would be delivered and recorded rather than
	// swallowed a second time.
	reject.disarm()
	b.evict(ClientID(testNodeID))

	select {
	case err := <-pubErr:
		// Under a session that survives the drop this returns nil: the packet is
		// retransmitted on the new connection and acknowledged there.
		if err == nil {
			t.Error("the abandoned publish reported success; the broker never acknowledged it on the connection it was made over")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned publish never returned")
	}

	b.waitFor("birth after reconnect", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 2
	})

	for _, m := range b.settle(300 * time.Millisecond) {
		if m.payload == outageValue {
			t.Errorf("the abandoned reading was replayed from session state after reconnecting: %s", m)
		}
	}
	if got, ok := b.retained(stateTopic); ok {
		t.Errorf("retained state = %q, want nothing at all", got)
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestShutdownWaitsForAnInFlightBirthSequence pins the quiesce step.
//
// The closing flag stops a *future* birth publish; quiesce is what deals with
// one already under way. A connection-up callback that has passed the flag
// check is a publish this package can no longer stop, so Shutdown waits a slice
// of its budget for the sequence to finish before it publishes "offline" — an
// "online" landing after "offline" is precisely the stuck-status defect, and
// the two publishes racing is the only way left to produce it.
//
// The wait is bounded, and TestLateBirthCannotOverwriteOffline covers the other
// side: a sequence that never finishes must not hold the teardown up.
func TestShutdownWaitsForAnInFlightBirthSequence(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	// A quarter of this — 500ms — is what quiesce gets, comfortably more than
	// the 100ms the callback below takes.
	p := newPublisher(t, addr, func(c *Config) { c.ShutdownTimeout = 2 * time.Second })

	entered := make(chan struct{})
	done := make(chan struct{})
	var offlineSeenMidSequence atomic.Bool
	p.OnConnectionUp(func() {
		defer close(done)
		close(entered)
		// Stands in for the round trips a real birth sequence makes between the
		// first publish and the last.
		time.Sleep(100 * time.Millisecond)
		offlineSeenMidSequence.Store(count(b.observed(), statusTopic, PayloadOffline) > 0)
	})

	mustStart(t, p)
	mustConnect(t, p)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("birth sequence never started")
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	<-done

	if offlineSeenMidSequence.Load() {
		t.Error(`Shutdown published "offline" while a birth sequence was still running; ` +
			`a publish that sequence had already started could land after it`)
	}
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q", got, ok, PayloadOffline)
	}
}

// TestStartDetachesTheConnectionFromTheCallersContext pins Start's
// context.WithoutCancel.
//
// In production Start is handed main's signal context, and the shape that
// matters is the one every test would otherwise miss: SIGTERM cancels that
// context, and only then does the deferred Shutdown run. If the connection
// manager were a child of it, autopaho would react to the cancellation by
// sending a clean DISCONNECT of its own — reason code 0x00, will discarded —
// while Shutdown was still trying to publish "offline" over a connection that
// was being torn down underneath it. Both deliveries of "offline" would be lost
// from one shutdown, which is exactly what the Python did.
func TestStartDetachesTheConnectionFromTheCallersContext(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	birth(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mustConnect(t, p)
	b.waitFor("birth online", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	cancel() // SIGTERM

	select {
	case <-p.cm.Load().Done():
		t.Fatal("the connection manager stopped when Start's context was cancelled; Shutdown would have had no connection left to publish \"offline\" over")
	case <-time.After(250 * time.Millisecond):
	}

	pubCtx, cancelPub := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPub()
	if err := p.PublishRetained(pubCtx, stateTopic, []byte("1")); err != nil {
		t.Errorf("publish after the caller's context was cancelled = %v, want nil", err)
	}

	// Shutdown gets the cancelled context, as it does in main.
	if err := p.Shutdown(ctx, statusTopic); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	assertRetainedOnce(t, b.settle(300*time.Millisecond), statusTopic, PayloadOffline)
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q", got, ok, PayloadOffline)
	}
}

// TestStartIsNonBlockingWithoutBroker covers the ordering bug in the Python:
// its 36-second serial-open retry ran before MQTT was set up, so no will was
// registered during exactly the window where the daemon was most likely to die.
// Start must return immediately whether or not a broker exists, and connect on
// its own once one does.
func TestStartIsNonBlockingWithoutBroker(t *testing.T) {
	addr := freeAddr(t) // deliberately nothing listening here

	p := newPublisher(t, addr, nil)
	birth(t, p)

	started := time.Now()
	mustStart(t, p)
	if d := time.Since(started); d > 500*time.Millisecond {
		t.Errorf("Start blocked for %s with no broker; it must not wait for a connection", d)
	}

	awaitCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := p.AwaitConnection(awaitCtx); err == nil {
		t.Error("AwaitConnection returned nil with no broker running")
	}

	b := startBroker(t, addr)
	b.watch(wildcard)

	mustConnect(t, p)
	b.waitFor("birth after the broker appears", 10*time.Second, func(msgs []message) bool {
		return count(msgs, statusTopic, PayloadOnline) == 1
	})

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestShutdownIsIdempotent guards the deferred-Shutdown-plus-explicit-Shutdown
// shape a main function tends to grow.
func TestShutdownIsIdempotent(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, nil)
	mustStart(t, p)
	mustConnect(t, p)

	first := p.Shutdown(sigtermContext(), statusTopic)
	second := p.Shutdown(sigtermContext(), statusTopic)
	if first != nil || second != nil {
		t.Fatalf("Shutdown errors = %v, %v; want nil, nil", first, second)
	}

	msgs := b.settle(300 * time.Millisecond)
	if got := count(msgs, statusTopic, PayloadOffline); got != 1 {
		t.Errorf("observed %d %q publishes after two Shutdowns, want 1: %s", got, PayloadOffline, format(msgs))
	}
	if err := p.PublishRetained(context.Background(), stateTopic, []byte("1")); !errors.Is(err, ErrShutdown) {
		t.Errorf("publish after Shutdown = %v, want %v", err, ErrShutdown)
	}
}

// TestSubscribeDeliversAndSurvivesReconnect: subscriptions are not restored by
// the broker because the session is created with CleanStart, so the Publisher
// has to re-issue them itself on every connection.
func TestSubscribeDeliversAndSurvivesReconnect(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)

	p := newPublisher(t, addr, nil)
	mustStart(t, p)
	mustConnect(t, p)

	received := make(chan string, 8)
	cmdTopic := baseTopic + "/sensor/ppfd/command"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Subscribe(ctx, cmdTopic, func(_ string, payload []byte) {
		received <- string(payload)
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	b.publish(cmdTopic, []byte("first"))
	expectReceive(t, received, "first")

	b.evict(ClientID(testNodeID))
	mustConnect(t, p)

	// The re-subscribe happens on a goroutine after the connection comes up, so
	// retry the publish until it lands rather than racing it once.
	waitUntil(t, "delivery after reconnect", 10*time.Second, func() bool {
		b.publish(cmdTopic, []byte("second"))
		select {
		case got := <-received:
			if got != "second" {
				t.Fatalf("received %q, want %q", got, "second")
			}
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func expectReceive(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("received %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// TestLateBirthCannotOverwriteOffline covers the nastiest ordering window: a
// reconnection lands, its birth sequence starts publishing, and SIGTERM arrives
// mid-sequence. If the birth's "online" were allowed to land after Shutdown's
// "offline", the retained status would say online for a process that has
// already exited — the original defect, reintroduced from the other direction.
//
// Shutdown waits a slice of its budget for the sequence to finish, then
// proceeds regardless; the closing flag is what makes proceeding safe.
func TestLateBirthCannotOverwriteOffline(t *testing.T) {
	addr := freeAddr(t)
	b := startBroker(t, addr)
	b.watch(wildcard)

	p := newPublisher(t, addr, func(c *Config) {
		// A quarter of this (100ms) is how long Shutdown waits for the birth
		// sequence below, which never finishes on its own.
		c.ShutdownTimeout = 400 * time.Millisecond
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var lateErr error
	p.OnConnectionUp(func() {
		close(entered)
		<-release
		lateErr = p.PublishRetained(context.Background(), statusTopic, []byte(PayloadOnline))
		close(done)
	})

	mustStart(t, p)
	mustConnect(t, p)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("birth sequence never started")
	}

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	close(release)
	<-done

	if !errors.Is(lateErr, ErrShutdown) {
		t.Errorf("late birth publish = %v, want %v", lateErr, ErrShutdown)
	}
	if got, ok := b.retained(statusTopic); !ok || got != PayloadOffline {
		t.Errorf("retained status = %q (present=%v), want %q", got, ok, PayloadOffline)
	}
}

// TestLifecycleErrors covers the states a caller can reach without a broker.
func TestLifecycleErrors(t *testing.T) {
	t.Run("publish before start", func(t *testing.T) {
		p := newPublisher(t, "127.0.0.1:1883", nil)
		if err := p.PublishRetained(context.Background(), stateTopic, []byte("1")); !errors.Is(err, ErrNotStarted) {
			t.Errorf("publish = %v, want %v", err, ErrNotStarted)
		}
		if err := p.AwaitConnection(context.Background()); !errors.Is(err, ErrNotStarted) {
			t.Errorf("await = %v, want %v", err, ErrNotStarted)
		}
	})

	t.Run("shutdown before start", func(t *testing.T) {
		p := newPublisher(t, "127.0.0.1:1883", nil)
		if err := p.Shutdown(sigtermContext(), statusTopic); !errors.Is(err, ErrNotStarted) {
			t.Errorf("shutdown = %v, want %v", err, ErrNotStarted)
		}
	})

	t.Run("start twice", func(t *testing.T) {
		p := newPublisher(t, freeAddr(t), nil)
		mustStart(t, p)
		defer func() { _ = p.Shutdown(sigtermContext(), statusTopic) }()
		if err := p.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
			t.Errorf("second Start = %v, want %v", err, ErrAlreadyStarted)
		}
	})

	t.Run("start with a cancelled context", func(t *testing.T) {
		p := newPublisher(t, freeAddr(t), nil)
		if err := p.Start(sigtermContext()); !errors.Is(err, context.Canceled) {
			t.Errorf("Start = %v, want context.Canceled", err)
		}
	})

	t.Run("subscribe validation", func(t *testing.T) {
		p := newPublisher(t, freeAddr(t), nil)
		if err := p.Subscribe(context.Background(), "", func(string, []byte) {}); err == nil {
			t.Error("empty filter accepted")
		}
		if err := p.Subscribe(context.Background(), "a/b", nil); err == nil {
			t.Error("nil callback accepted")
		}
		// Before Start, a subscription is recorded rather than rejected: the
		// connection-up sequence issues it.
		if err := p.Subscribe(context.Background(), "a/b", func(string, []byte) {}); err != nil {
			t.Errorf("Subscribe before Start = %v, want nil", err)
		}
	})

	t.Run("subscribe while disconnected", func(t *testing.T) {
		// Started, but no broker exists: the filter is recorded for the
		// connection-up sequence rather than reported as a failure. autopaho has
		// no client at all in this state, so this is the ConnectionDownError
		// half of connectionUnavailable.
		p := newPublisher(t, freeAddr(t), nil)
		mustStart(t, p)
		defer func() { _ = p.Shutdown(sigtermContext(), statusTopic) }()
		if err := p.Subscribe(context.Background(), "a/b", func(string, []byte) {}); err != nil {
			t.Errorf("Subscribe while disconnected = %v, want nil", err)
		}
	})

	t.Run("subscribe while reconnecting", func(t *testing.T) {
		// The other half. Once a connection has existed, autopaho keeps its paho
		// client until that client has finished shutting down, so a subscribe
		// issued after the session went away but before the client did reaches
		// paho and comes back as session.ErrNoConnection — which
		// autopaho.ConnectionDownError does not wrap.
		//
		// Cancelling the connection manager reproduces that state exactly, and
		// permanently: autopaho leaves c.cli set when its context ends
		// (autopaho/auto.go:341-356), so what would otherwise be a window of
		// microseconds on every reconnection is here a steady state.
		addr := freeAddr(t)
		startBroker(t, addr)
		p := newPublisher(t, addr, nil)
		mustStart(t, p)
		mustConnect(t, p)

		p.mu.Lock()
		cancelConn := p.cancelConn
		p.mu.Unlock()
		cancelConn()
		<-p.cm.Load().Done()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Subscribe(ctx, "a/b", func(string, []byte) {}); err != nil {
			t.Errorf("Subscribe while reconnecting = %v, want nil (the filter is recorded and re-issued on connection up)", err)
		}
	})

	t.Run("nil connection-up callback ignored", func(t *testing.T) {
		p := newPublisher(t, freeAddr(t), nil)
		p.OnConnectionUp(nil)
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.upFns) != 0 {
			t.Errorf("registered %d callbacks, want 0", len(p.upFns))
		}
	})
}

// TestLogAdapter checks the bridge from paho's Println/Printf logger onto slog,
// which is the only way connection failures become visible in the journal.
func TestLogAdapter(t *testing.T) {
	var buf strings.Builder
	a := logAdapter{slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), slog.LevelWarn}

	a.Println("connection", "lost")
	a.Printf("retrying in %ds", 5)

	got := buf.String()
	for _, want := range []string{"connection lost", "retrying in 5s", "level=WARN"} {
		if !strings.Contains(got, want) {
			t.Errorf("log output %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, `\n`) {
		t.Errorf("log output %q retains the trailing newline Println adds", got)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{Host: "h", Port: 1883, NodeID: "n", StatusTopic: "s"}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"no host", func(c *Config) { c.Host = "" }, "Host"},
		{"port zero", func(c *Config) { c.Port = 0 }, "Port"},
		{"port too high", func(c *Config) { c.Port = 65536 }, "Port"},
		{"no node id", func(c *Config) { c.NodeID = "" }, "NodeID"},
		{"no status topic", func(c *Config) { c.StatusTopic = "" }, "StatusTopic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			_, err := New(cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("New: %v", err)
			case tt.wantErr == "":
				return
			case err == nil:
				t.Fatalf("New succeeded, want an error mentioning %q", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("New error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestConfigDefaults asserts the literal values rather than the constants that
// hold them. Comparing p.cfg.KeepAlive to defaultKeepAlive is a statement about
// withDefaults and nothing else: it would sit there green while the value went
// back to the Python's 30 s, which is a measured decision, not a preference.
func TestConfigDefaults(t *testing.T) {
	p, err := New(Config{Host: "broker", Port: 1883, NodeID: "n", StatusTopic: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Half the Python's 30. The broker declares a client dead after 1.5
	// keepalives, so this is what bounds worst-case will latency to ~15 s on a
	// wedged link — one where there is no FIN for the broker to notice.
	if got, want := p.cfg.KeepAlive, uint16(10); got != want {
		t.Errorf("default KeepAlive = %ds, want %ds (worst-case will latency is 1.5x this)", got, want)
	}
	if got, want := p.cfg.ConnectRetryDelay, 5*time.Second; got != want {
		t.Errorf("default ConnectRetryDelay = %s, want %s", got, want)
	}
	if got, want := p.cfg.ConnectTimeout, 10*time.Second; got != want {
		t.Errorf("default ConnectTimeout = %s, want %s", got, want)
	}
	// Shutdown's whole budget, and systemd's TimeoutStopSec has to stay well
	// clear of it.
	if got, want := p.cfg.ShutdownTimeout, 3*time.Second; got != want {
		t.Errorf("default ShutdownTimeout = %s, want %s", got, want)
	}
	if got, want := p.url.String(), "mqtt://broker:1883"; got != want {
		t.Errorf("url = %s, want %s", got, want)
	}
}

// TestConnectionUnavailable pins both halves of the "not an error, just a
// state" classification that Subscribe leans on. They come from different
// packages and neither wraps the other.
func TestConnectionUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"autopaho has no client", autopaho.ConnectionDownError, true},
		{"paho session has no connection", session.ErrNoConnection, true},
		{"wrapped, as sendSubscribe returns it", fmt.Errorf("mqttpub: subscribe %q: %w", "a/b", session.ErrNoConnection), true},
		{"wrapped connection down", fmt.Errorf("mqttpub: subscribe %q: %w", "a/b", autopaho.ConnectionDownError), true},
		// A broker that refuses the filter, and a caller's own deadline, are
		// both real failures the caller has to hear about.
		{"the broker refused the filter", errors.New("mqttpub: subscribe \"a/b\": failed to subscribe: not authorized"), false},
		{"the caller's deadline", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectionUnavailable(tt.err); got != tt.want {
				t.Errorf("connectionUnavailable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestServerURLHandlesIPv6 — the broker address comes from an env var, and an
// IPv6 literal there would otherwise produce an unparseable URL.
func TestServerURLHandlesIPv6(t *testing.T) {
	p, err := New(Config{Host: "fd00::1", Port: 1883, NodeID: "n", StatusTopic: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := p.url.String(), "mqtt://[fd00::1]:1883"; got != want {
		t.Errorf("url = %s, want %s", got, want)
	}
}

func TestClientID(t *testing.T) {
	if got, want := ClientID("quantum-sensor"), "apogee-quantum-sensor"; got != want {
		t.Errorf("ClientID = %q, want %q", got, want)
	}
}

// TestDisconnectPacket is the unit-level statement of the rule the integration
// tests verify end to end.
func TestDisconnectPacket(t *testing.T) {
	p, err := New(Config{Host: "h", Port: 1883, NodeID: "n", StatusTopic: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := p.disconnectPacket().ReasonCode; got != disconnectWithWill {
		t.Errorf("reason code before offline is confirmed = %#x, want %#x (disconnect with will)", got, disconnectWithWill)
	}
	p.offlineConfirmed.Store(true)
	if got := p.disconnectPacket().ReasonCode; got != disconnectNormal {
		t.Errorf("reason code after offline is confirmed = %#x, want %#x (normal disconnection)", got, disconnectNormal)
	}
}

// TestARepeatedConnectionFailureIsLoggedOnce is the same defect class as the
// daemon's M4 and HIGH 5, in the one place that will actually bury a journal
// during a broker partition.
//
// OnConnectError fires once per attempt and autopaho retries on
// ConnectRetryDelay forever, so an unguarded line here is one per retry for as
// long as the broker is away: measured at 21 lines in 105 s of outage, i.e.
// ~17,280 a day at the production 5 s delay. Everything else that repeats in
// this daemon — the probe warnings, the reopen notice, the publish failure, the
// watchdog ping — is transition-guarded; this was the last one that was not.
func TestARepeatedConnectionFailureIsLoggedOnce(t *testing.T) {
	addr := freeAddr(t) // deliberately nothing listening
	journal := newLogSink(t)

	p := newPublisher(t, addr, func(c *Config) {
		c.ConnectRetryDelay = 20 * time.Millisecond
		c.ConnectTimeout = 100 * time.Millisecond
		c.Logger = journal.logger()
	})
	mustStart(t, p)

	// Long enough for well over a dozen attempts at a 20 ms retry delay.
	const attempts = 15
	deadline := time.Now().Add(5 * time.Second)
	for journal.count("mqtt connection attempt") < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Duration(attempts) * 20 * time.Millisecond)

	if n := journal.count("mqtt connection attempt failed"); n > 1 {
		t.Errorf("%q appears %d times across ~%d attempts, want 1; at the production 5s retry that is ~%d a day:\n%s",
			"mqtt connection attempt failed", n, attempts, 24*60*60/5, journal.dump())
	}
	if n := journal.count("level=WARN"); n > 1 {
		t.Errorf("%d warnings for one unreachable broker:\n%s", n, journal.dump())
	}

	// And it says so when it recovers, or a suppressed line and a fixed broker
	// are indistinguishable.
	b := startBroker(t, addr)
	b.watch(wildcard)
	mustConnect(t, p)
	waitForLine(t, journal, "mqtt connection recovered")

	if err := p.Shutdown(sigtermContext(), statusTopic); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// waitForLine blocks until substr appears in the captured journal.
func waitForLine(t *testing.T, journal *logSink, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if journal.count(substr) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in the journal:\n%s", substr, journal.dump())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// logSink captures what the daemon would write to the journal, at the level the
// process actually runs at. Several of the defects this package guards against
// are log-volume defects, which makes "how many times does this line appear" a
// property worth a test.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func newLogSink(t *testing.T) *logSink {
	t.Helper()
	return &logSink{}
}

func (s *logSink) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.lines = append(s.lines, strings.TrimRight(string(p), "\n"))
	s.mu.Unlock()
	return len(p), nil
}

func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, line := range s.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func (s *logSink) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return "(nothing)"
	}
	return "  " + strings.Join(s.lines, "\n  ")
}

package mqttpub

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// The tests in this package are integration tests against a real MQTT broker
// running in-process. Nothing here mocks autopaho: the assertions are all about
// what the *broker* observed, because every bug this package exists to prevent
// is one where the client believed it had published something the broker never
// saw (or vice versa — the Python's clean disconnect made the broker discard a
// will the client was counting on).
//
// Observation goes through mochi's inline client rather than a second autopaho
// connection, so the observer can never be the thing under test.

// message is one delivery observed by the inline subscriber.
type message struct {
	topic   string
	payload string
	retain  bool
}

func (m message) String() string { return fmt.Sprintf("%s=%q", m.topic, m.payload) }

// broker is an embedded MQTT server plus a recording of everything published to
// the filters it was told to watch.
type broker struct {
	t *testing.T
	// srv is written once, before the broker is handed to the test, and never
	// again — stop() records the fact in the guarded stopped flag instead of
	// clearing this. Tests do stop brokers mid-run
	// (TestPublishWhileBrokerDownFailsAndDoesNotQueue), and a field that
	// mutates would be a data race against every reader, plus a nil dereference
	// for whoever observed the broker afterwards.
	srv  *mqtt.Server
	addr string

	mu      sync.Mutex
	msgs    []message
	stopped bool

	nextSubID int
}

// freeAddr reserves a loopback port and releases it, so a broker can be started
// on that address later. Tests that stop and restart a broker (or start the
// publisher before the broker exists) need the address to be known up front.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// startBroker runs an embedded broker on addr until the test ends.
func startBroker(t *testing.T, addr string, hooks ...mqtt.Hook) *broker {
	t.Helper()

	srv := mqtt.New(&mqtt.Options{
		InlineClient: true, // required for the observer subscriptions below
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatalf("add auth hook: %v", err)
	}
	for _, h := range hooks {
		if err := srv.AddHook(h, nil); err != nil {
			t.Fatalf("add hook %s: %v", h.ID(), err)
		}
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: addr})); err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}

	b := &broker{t: t, srv: srv, addr: addr}
	go func() {
		if err := srv.Serve(); err != nil {
			panic(fmt.Sprintf("broker serve: %v", err))
		}
	}()
	t.Cleanup(b.stop)
	return b
}

// stop shuts the broker down. Safe to call more than once, which matters for
// tests that stop the broker mid-test and still have the cleanup registered —
// mochi's Close closes a channel, so a second one would panic.
func (b *broker) stop() {
	b.mu.Lock()
	already := b.stopped
	b.stopped = true
	b.mu.Unlock()
	if already {
		return
	}

	// Bounded, because mochi v2.7.9 can deadlock inside Close.
	//
	// Server.Close -> closeListenerClients -> Clients.GetByListener -> Clients.Len
	// takes an RLock, while a client arriving at that instant runs
	// EstablishConnection -> attachClient -> Clients.Delete, which wants the
	// Lock. Go's RWMutex is write-preferring, so the pending Lock parks the
	// RLock behind it and neither side ever runs.
	//
	// This is reachable here rather than theoretical: t.Cleanup is LIFO, so a
	// test that starts a second broker has that broker closed while the
	// publisher is still up and retrying every ConnectRetryDelay (100ms) — an
	// EstablishConnection is very likely in flight. It cost a full `go test`
	// run a 10-minute timeout and a stack dump pointing into a vendored
	// broker, on a change that touched none of this.
	//
	// Cleanup runs after the test has already reached its verdict, so waiting
	// out an upstream deadlock buys nothing. Report it and move on: the
	// goroutine dies with the test binary. Deliberately not a t.Error — a
	// vendored broker's shutdown race is not this package's failure, and a
	// flaky red teaches people to ignore the suite.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.srv.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		b.t.Logf("broker on %s did not shut down within 5s; this is the known mochi v2.7.9 "+
			"Close/EstablishConnection deadlock, not a fault in the code under test", b.addr)
	}
}

// server returns the running server, and is the only way to reach it: a stopped
// broker still answers Subscribe and Publish, silently and pointlessly, so
// asking one a question is a test bug and should read as one.
func (b *broker) server() *mqtt.Server {
	b.t.Helper()
	b.mu.Lock()
	stopped := b.stopped
	b.mu.Unlock()
	if stopped {
		b.t.Fatalf("broker on %s has been stopped; it cannot be observed or driven any more", b.addr)
	}
	return b.srv
}

// subID hands out inline-subscription identifiers, which must be unique per
// broker and are allocated from more than one goroutine's worth of test code.
func (b *broker) subID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSubID++
	return b.nextSubID
}

// watch records every message published to filter. Call it before the publisher
// connects: an inline subscription also receives retained messages at
// subscription time, so subscribing late would double-count.
func (b *broker) watch(filter string) {
	b.t.Helper()
	err := b.server().Subscribe(filter, b.subID(), func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.msgs = append(b.msgs, message{
			topic:   pk.TopicName,
			payload: string(pk.Payload),
			retain:  pk.FixedHeader.Retain,
		})
	})
	if err != nil {
		b.t.Fatalf("watch %q: %v", filter, err)
	}
}

// evict disconnects one client from the broker side, which is as close to a
// real drop as this harness gets: the client sees the connection go away and
// reconnects on its own.
func (b *broker) evict(clientID string) {
	b.t.Helper()
	srv := b.server()
	cl, ok := srv.Clients.Get(clientID)
	if !ok {
		b.t.Fatalf("broker does not know client %q", clientID)
	}
	if err := srv.DisconnectClient(cl, packets.CodeDisconnect); err != nil {
		b.t.Fatalf("disconnect client %q: %v", clientID, err)
	}
}

// publish sends a message from the broker's inline client — how the tests
// deliver commands to the daemon's subscriptions.
func (b *broker) publish(topic string, payload []byte) {
	b.t.Helper()
	if err := b.server().Publish(topic, payload, false, 0); err != nil {
		b.t.Fatalf("broker publish to %q: %v", topic, err)
	}
}

// observed returns a snapshot of every recorded message, in arrival order.
func (b *broker) observed() []message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]message(nil), b.msgs...)
}

// payloads returns the payloads recorded for one topic, in arrival order.
func (b *broker) payloads(topic string) []string {
	var out []string
	for _, m := range b.observed() {
		if m.topic == topic {
			out = append(out, m.payload)
		}
	}
	return out
}

// retained asks the broker what it would hand a fresh subscriber for topic —
// i.e. the retained message actually held in its store, which is the thing
// grow-app reads on startup. Returns ok=false if nothing is retained.
//
// Only deliveries carrying the retain flag count. A message published live in
// the same instant reaches this subscription too, and accepting one of those
// would report a value the broker is not actually holding.
func (b *broker) retained(topic string) (string, bool) {
	b.t.Helper()

	srv := b.server()
	got := make(chan string, 4)
	id := b.subID()
	err := srv.Subscribe(topic, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		if !pk.FixedHeader.Retain {
			return
		}
		select {
		case got <- string(pk.Payload):
		default:
		}
	})
	if err != nil {
		b.t.Fatalf("retained %q: %v", topic, err)
	}
	defer func() { _ = srv.Unsubscribe(topic, id) }()

	select {
	case p := <-got:
		return p, true
	case <-time.After(time.Second):
		return "", false
	}
}

// waitFor polls until cond is satisfied, failing the test if it never is.
// Polling (rather than a signal per message) keeps the conditions readable —
// they are all statements about the accumulated recording.
func (b *broker) waitFor(what string, timeout time.Duration, cond func([]message) bool) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs := b.observed()
		if cond(msgs) {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out after %s waiting for %s; observed: %s", timeout, what, format(msgs))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle gives the broker time to receive anything else that might be in
// flight, then returns the recording. Used to assert that something did *not*
// happen — a duplicate "offline" from a will that should have been suppressed
// arrives immediately after the one we asked for, not seconds later.
func (b *broker) settle(d time.Duration) []message {
	time.Sleep(d)
	return b.observed()
}

func format(msgs []message) string {
	if len(msgs) == 0 {
		return "(nothing)"
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.String()
	}
	return strings.Join(parts, ", ")
}

func count(msgs []message, topic, payload string) int {
	return len(retainFlags(msgs, topic, payload))
}

// retainFlags returns the retain flag of each recorded message matching topic
// and payload, in arrival order — so a test can say both "how many" and "were
// they retained".
//
// The retain flag is not decoration here. grow-app reads the availability topic
// from the broker's retained store when it starts; an "offline" published
// without it would be delivered to whoever happened to be connected at that
// instant and then forgotten, leaving the previous retained "online" in place.
// mochi hands inline subscribers the packet as the publisher sent it, so this
// is the flag that went over the wire.
func retainFlags(msgs []message, topic, payload string) []bool {
	var out []bool
	for _, m := range msgs {
		if m.topic == topic && m.payload == payload {
			out = append(out, m.retain)
		}
	}
	return out
}

// assertRetainedOnce fails unless exactly one matching message was recorded and
// it carried the retain flag.
func assertRetainedOnce(t *testing.T, msgs []message, topic, payload string) {
	t.Helper()
	flags := retainFlags(msgs, topic, payload)
	if len(flags) != 1 {
		t.Errorf("observed %d %q publishes on %s, want exactly 1: %s", len(flags), payload, topic, format(msgs))
		return
	}
	if !flags[0] {
		t.Errorf("%q was published to %s without the retain flag; grow-app reads the retained status on startup", payload, topic)
	}
}

// rejectHook drops PUBLISHes to one topic without acknowledging them, which is
// how the tests simulate "the offline publish failed" while leaving the
// connection perfectly healthy. That isolation matters: it means the will fired
// because of our DISCONNECT reason code, not because the socket dropped.
//
// A rejected publish is also never acknowledged, so it stays in the client's
// session state — which is what lets a test hold a publish open across a
// connection drop and then watch for a replay. disarm() stops the interception
// so the replay, if there is one, is delivered and recorded.
type rejectHook struct {
	mqtt.HookBase
	topic   string
	payload string // empty rejects every payload on topic

	mu       sync.Mutex
	disarmed bool
	rejects  int
}

func (h *rejectHook) ID() string { return "reject-publish" }

func (h *rejectHook) Provides(b byte) bool { return b == mqtt.OnPublish }

func (h *rejectHook) OnPublish(_ *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if pk.TopicName != h.topic || (h.payload != "" && string(pk.Payload) != h.payload) {
		return pk, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.disarmed {
		return pk, nil
	}
	h.rejects++
	return pk, packets.ErrRejectPacket
}

// rejected reports how many publishes the hook has dropped.
func (h *rejectHook) rejected() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rejects
}

// disarm lets matching publishes through from now on.
func (h *rejectHook) disarm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.disarmed = true
}

// eventHook records the order in which the broker processed SUBSCRIBE and
// PUBLISH packets from one client. It is the only way to assert the *package's*
// connection-up ordering rather than the ordering of whatever a test's own
// callback happens to do: both events are round trips, so the sequence the
// broker saw is the sequence the Publisher issued.
type eventHook struct {
	mqtt.HookBase
	clientID string

	mu     sync.Mutex
	events []string
}

func (h *eventHook) ID() string { return "event-order" }

func (h *eventHook) Provides(b byte) bool { return b == mqtt.OnSubscribed || b == mqtt.OnPublished }

func (h *eventHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, _ []byte) {
	for _, f := range pk.Filters {
		h.record(cl, "subscribe "+f.Filter)
	}
}

func (h *eventHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	h.record(cl, "publish "+pk.TopicName)
}

func (h *eventHook) record(cl *mqtt.Client, event string) {
	if cl.ID != h.clientID { // ignore the inline observer's own subscriptions
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

// recorded returns a snapshot of the events, in broker order.
func (h *eventHook) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

// The will bypasses this hook entirely: mochi's sendLWT retains and fans out the
// packet directly rather than routing it through the inbound publish path. That
// is what makes the hook usable to fail one specific publish while leaving the
// will able to deliver the same payload.

// testLogger routes the Publisher's logging into t.Log, with a guard against
// writing after the test finishes — autopaho's goroutines outlive some of these
// tests by a few milliseconds, and a stray t.Log then panics the whole run.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	w := &testWriter{t: t}
	t.Cleanup(w.done)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type testWriter struct {
	mu       sync.Mutex
	t        *testing.T
	finished bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.finished {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func (w *testWriter) done() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = true
}

package app

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
)

// An embedded MQTT broker, so the end-to-end test can assert on what the
// *broker* observed rather than on what a fake recorded. It mirrors the helper
// in internal/mqttpub, which is test-only and therefore not importable; the
// duplication buys the one thing a fake cannot — proof that the birth sequence
// survives autopaho, paho's session state and a real retained store.

type brokerMessage struct {
	topic   string
	payload string
	retain  bool
}

func (m brokerMessage) String() string { return fmt.Sprintf("%s=%q", m.topic, m.payload) }

type testBroker struct {
	t    *testing.T
	srv  *mqtt.Server
	addr string

	mu      sync.Mutex
	msgs    []brokerMessage
	stopped bool
	nextSub int
}

// startBroker runs an embedded broker on a free loopback port until the test
// ends.
func startBroker(t *testing.T) *testBroker {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	srv := mqtt.New(&mqtt.Options{
		InlineClient: true, // required for the observer subscriptions below
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatalf("add auth hook: %v", err)
	}
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "t", Address: addr})); err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}

	b := &testBroker{t: t, srv: srv, addr: addr}
	go func() {
		if err := srv.Serve(); err != nil {
			panic(fmt.Sprintf("broker serve: %v", err))
		}
	}()
	t.Cleanup(b.stop)
	return b
}

func (b *testBroker) stop() {
	b.mu.Lock()
	already := b.stopped
	b.stopped = true
	b.mu.Unlock()
	if !already {
		_ = b.srv.Close()
	}
}

func (b *testBroker) host() (string, int) {
	host, port, err := net.SplitHostPort(b.addr)
	if err != nil {
		b.t.Fatalf("split %s: %v", b.addr, err)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		b.t.Fatalf("parse port %s: %v", port, err)
	}
	return host, n
}

// watch records every message published under filter. Call it before the
// publisher connects: an inline subscription also receives retained messages at
// subscription time, so subscribing late would double-count.
func (b *testBroker) watch(filter string) {
	b.t.Helper()
	b.mu.Lock()
	b.nextSub++
	id := b.nextSub
	b.mu.Unlock()

	err := b.srv.Subscribe(filter, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.msgs = append(b.msgs, brokerMessage{
			topic:   pk.TopicName,
			payload: string(pk.Payload),
			retain:  pk.FixedHeader.Retain,
		})
	})
	if err != nil {
		b.t.Fatalf("watch %q: %v", filter, err)
	}
}

// publish sends a message from the broker's inline client — how a test delivers
// a command to the daemon's subscriptions.
func (b *testBroker) publish(topic string, payload []byte) {
	b.t.Helper()
	if err := b.srv.Publish(topic, payload, false, 0); err != nil {
		b.t.Fatalf("broker publish to %q: %v", topic, err)
	}
}

func (b *testBroker) observed() []brokerMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]brokerMessage(nil), b.msgs...)
}

// retained asks the broker what it would hand a fresh subscriber for topic —
// the retained message actually held in its store, which is what grow-app reads
// on startup.
//
// THE TRAP this closes: the subscription stays live until Unsubscribe, and a
// message published with the retain flag is delivered to it carrying that flag
// too, so waiting on the callback for a while cannot tell the store's answer
// from traffic that merely happened to arrive. The daemon re-asserts every
// retraction on a reconnection birth, and a connection-up landing just after the
// poller's own birth put a second empty-payload publish on tilt/config right
// inside that window — which read as "the store holds an empty message", a state
// mochi never leaves it in, because RetainMessage deletes the entry when the
// payload is empty. One run in twenty failed on it.
//
// mochi hands the store over synchronously from inside Subscribe (server.go,
// "Handling retained messages", walking Topics.Messages(filter)), so sealing the
// handler the moment Subscribe returns asks about the store and nothing else.
// The polling around it is what the old one-second receive was really providing:
// the daemon's births run on their own goroutines, so a message this test is
// waiting for may not have been published yet.
func (b *testBroker) retained(topic string) (string, bool) {
	b.t.Helper()

	deadline := time.Now().Add(time.Second)
	for {
		if payload, ok := b.retainedNow(topic); ok {
			return payload, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// retainedNow is one look at the retained store.
func (b *testBroker) retainedNow(topic string) (string, bool) {
	b.t.Helper()

	b.mu.Lock()
	b.nextSub++
	id := b.nextSub
	b.mu.Unlock()

	var (
		mu      sync.Mutex
		sealed  bool
		payload string
		found   bool
	)
	err := b.srv.Subscribe(topic, id, func(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
		mu.Lock()
		defer mu.Unlock()
		if sealed || !pk.FixedHeader.Retain {
			return
		}
		payload, found = string(pk.Payload), true
	})
	if err != nil {
		b.t.Fatalf("retained %q: %v", topic, err)
	}
	mu.Lock()
	sealed = true
	mu.Unlock()
	_ = b.srv.Unsubscribe(topic, id)

	return payload, found
}

// waitFor polls until cond is satisfied against the accumulated recording.
func (b *testBroker) waitFor(what string, timeout time.Duration, cond func([]brokerMessage) bool) {
	b.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs := b.observed()
		if cond(msgs) {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out after %s waiting for %s; observed:\n  %s", timeout, what, formatBrokerMsgs(msgs))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func formatBrokerMsgs(msgs []brokerMessage) string {
	if len(msgs) == 0 {
		return "(nothing)"
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.String()
	}
	return joinLines(out)
}

func joinLines(s []string) string {
	out := ""
	for i, line := range s {
		if i > 0 {
			out += "\n  "
		}
		out += line
	}
	return out
}

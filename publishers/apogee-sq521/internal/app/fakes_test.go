package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

// The fakes. Every one of them stands in for a seam in seams.go, which is what
// lets this package's tests cover the orderings the daemon exists to enforce
// without a serial port, a broker, a service manager or a wall clock.

// testConfig is a valid configuration with the production defaults, so a test
// that cares about one field can override just that one.
func testConfig() config.Config {
	return config.Config{
		NodeID:          "quantum-sensor",
		MQTTHost:        "127.0.0.1",
		MQTTPort:        1883,
		MQTTUsername:    "edge-daniel-home",
		MQTTPassword:    "secret",
		TopicPrefix:     "grow/daniel-home",
		SDI12Port:       "/dev/null",
		SDI12Address:    "0",
		PollInterval:    10 * time.Second,
		PPFDUnit:        "µmol/s/m²",
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    time.Second,
		OfflineAfter:    3,
		ShutdownTimeout: 3 * time.Second,
	}
}

// testRig is an App plus every fake it was built from.
type testRig struct {
	*App
	cfg    config.Config
	clock  *fakeClock
	dialer *fakeDialer
	pub    *fakePublisher
	notify *fakeNotifier
	zones  *fakeZones
	acc    *fakeIntegrator
	sink   *RolloverSink

	// journal, when set by withJournal, captures the daemon's log output at the
	// level the process runs at.
	journal *logSink
}

// withJournal routes the App's logging into a capturing sink, so a test can
// assert on the journal an operator would read.
func withJournal(t *testing.T) func(*testRig) {
	t.Helper()
	return func(r *testRig) { r.journal = newLogSink(t) }
}

// newRig builds an App around fakes. opts run against the rig's collaborators
// before New, so a test can script the session or change the config first.
func newRig(t *testing.T, opts ...func(*testRig)) *testRig {
	t.Helper()

	r := &testRig{
		cfg:    testConfig(),
		clock:  newFakeClock(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)),
		pub:    newFakePublisher(),
		notify: &fakeNotifier{},
		zones:  &fakeZones{loc: time.UTC},
		acc:    &fakeIntegrator{},
		sink:   &RolloverSink{},
	}
	r.dialer = &fakeDialer{fn: func(int) (Session, error) { return newHealthySession(), nil }}
	for _, opt := range opts {
		opt(r)
	}

	log := testLogger(t)
	if r.journal != nil {
		log = r.journal.logger()
	}
	a, err := New(r.cfg, Deps{
		Dialer:     r.dialer,
		Publisher:  r.pub,
		Notifier:   r.notify,
		Zones:      r.zones,
		Integrator: r.acc,
		Rollovers:  r.sink,
		Clock:      r.clock,
		Logger:     log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.App = a
	return r
}

// topic helpers, so tests read as statements about the wire rather than about
// string concatenation.
func (r *testRig) stateTopic(component, objectID string) string {
	return r.topics.State(component, objectID)
}

func (r *testRig) discoveryTopic(component, objectID string) string {
	return r.topics.Discovery(component, objectID)
}

func (r *testRig) statusTopic() string { return r.topics.Status() }

// ---------------------------------------------------------------- clock ----

// fakeClock drives every wait in the package. Advance fires the timers that
// have come due; awaitWaiters lets a test synchronise with a goroutine that is
// about to park on one.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter

	// onAdvance runs after every Advance, outside the clock's own lock, so a
	// test can assert an invariant at every instant time actually passes.
	onAdvance func(now time.Time)
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Capacity 1 and never blocking on delivery: every wait in the package
	// races this against ctx.Done(), so abandoned timers are the normal case.
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and fires everything that has come due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	kept := c.waiters[:0:0]
	var due []fakeWaiter
	for _, w := range c.waiters {
		if !w.at.After(now) {
			due = append(due, w)
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
	hook := c.onAdvance
	c.mu.Unlock()

	if hook != nil {
		hook(now)
	}
	for _, w := range due {
		w.ch <- now
	}
}

// pending returns the deadlines of every registered timer, which is how a test
// asserts *when* the poll loop decided to wake rather than merely that it did.
func (c *fakeClock) pending() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, 0, len(c.waiters))
	for _, w := range c.waiters {
		out = append(out, w.at)
	}
	return out
}

// awaitWaiters blocks until at least n timers are registered, so a test can
// advance the clock knowing the goroutine under test is parked rather than
// racing it.
func (c *fakeClock) awaitWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.waiters)
		c.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pending timers; have %d", n, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// -------------------------------------------------------------- session ----

// step is one scripted answer to a measurement command.
type step struct {
	vals []float64
	err  error
}

// fakeSession scripts an SDI-12 conversation. Each sub-command has a list of
// steps consumed in order; the last one repeats forever, which is what lets a
// test say "then it works from now on" without counting cycles.
type fakeSession struct {
	mu sync.Mutex

	name      string
	ident     sdi12.Identity
	identErr  error
	verify    []float64
	verifyErr error

	scripts map[string][]step
	at      map[string]int

	calls  []string
	closed int
	missed uint64

	// onMeasure runs before each Measure answers, so a test can advance the
	// clock inside a cycle or inject a race.
	onMeasure func(sub string, n int)
	// onCall runs at the top of every command, before the fake takes its lock,
	// so a test can drive a concurrent event at a precise point in the probe.
	onCall func(call string)
}

// newHealthySession is an SQ-521 that answers everything, tilt included.
func newHealthySession() *fakeSession {
	return &fakeSession{
		name:   "/dev/serial/by-id/fake -> /dev/ttyUSB0",
		ident:  sdi12.Identity{Address: '0', Version: "13", Vendor: "APOGEE", Model: "SQ-521", Firmware: "104", Serial: "3043"},
		verify: []float64{1.234567, 0},
		scripts: map[string][]step{
			"":  {{vals: []float64{156.9289}}},
			"1": {{vals: []float64{15.9124}}},
			"4": {{vals: []float64{1.2}}},
		},
	}
}

// script replaces one sub-command's answers.
func (s *fakeSession) script(sub string, steps ...step) *fakeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[sub] = steps
	return s
}

func (s *fakeSession) Identify(context.Context) (sdi12.Identity, error) {
	s.call("I")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "I")
	return s.ident, s.identErr
}

func (s *fakeSession) Verify(context.Context) ([]float64, error) {
	s.call("V")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "V")
	return s.verify, s.verifyErr
}

func (s *fakeSession) Measure(_ context.Context, sub string) ([]float64, error) {
	s.call("M" + sub)
	s.mu.Lock()
	s.calls = append(s.calls, "M"+sub)
	steps := s.scripts[sub]
	i := s.at[sub]
	if s.at == nil {
		s.at = map[string]int{}
	}
	if i < len(steps)-1 {
		s.at[sub] = i + 1
	}
	hook, n := s.onMeasure, i
	var cur step
	if len(steps) > 0 {
		cur = steps[min(i, len(steps)-1)]
	} else {
		cur = step{err: fmt.Errorf("fake: no script for %q: %w", sub, sdi12.ErrNoResponse)}
	}
	s.mu.Unlock()

	if hook != nil {
		hook(sub, n)
	}
	return cur.vals, cur.err
}

// call runs the onCall hook outside the fake's own lock, so a hook may reach
// back into the session it was installed on.
func (s *fakeSession) call(name string) {
	s.mu.Lock()
	hook := s.onCall
	s.mu.Unlock()
	if hook != nil {
		hook(name)
	}
}

func (s *fakeSession) MissedServiceRequests() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.missed
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *fakeSession) String() string { return s.name }

func (s *fakeSession) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *fakeSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// --------------------------------------------------------------- dialer ----

type fakeDialer struct {
	mu   sync.Mutex
	n    int
	fn   func(n int) (Session, error)
	made []Session
}

func (d *fakeDialer) Dial(ctx context.Context) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	n := d.n
	d.n++
	fn := d.fn
	d.mu.Unlock()

	s, err := fn(n)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.made = append(d.made, s)
	d.mu.Unlock()
	return s, nil
}

func (d *fakeDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *fakeDialer) sessions() []Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Session(nil), d.made...)
}

// ------------------------------------------------------------ publisher ----

// pubMsg is one publish, in arrival order.
type pubMsg struct {
	topic   string
	payload string
	// fromPoller records whether the publish arrived on a context the poll
	// goroutine had marked. It is the only reliable way for a test to tell
	// which goroutine a message came from, and it reads the same signal the
	// production code uses for the heartbeat rather than a stand-in for it.
	fromPoller bool
}

func (m pubMsg) String() string { return fmt.Sprintf("%s=%q", m.topic, m.payload) }

type fakePublisher struct {
	mu        sync.Mutex
	msgs      []pubMsg
	upFns     []func()
	subs      map[string]func(string, []byte)
	failure   error
	started   bool
	stopTopic string
	stopped   bool

	// onPublish runs before each publish is recorded, outside the publisher's
	// own lock, so a test can make a publish cost wall time.
	onPublish func(ctx context.Context, topic string)
	// delay makes each publish take real time, which is what lets a test open a
	// window two goroutines could interleave in.
	delay time.Duration
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{subs: map[string]func(string, []byte){}}
}

func (p *fakePublisher) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *fakePublisher) PublishRetained(ctx context.Context, topic string, payload []byte) error {
	p.mu.Lock()
	hook, delay := p.onPublish, p.delay
	p.mu.Unlock()
	if hook != nil {
		hook(ctx, topic)
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return p.failure
	}
	p.msgs = append(p.msgs, pubMsg{topic: topic, payload: string(payload), fromPoller: onPoller(ctx)})
	return nil
}

func (p *fakePublisher) Subscribe(_ context.Context, topic string, cb func(string, []byte)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs[topic] = cb
	return nil
}

func (p *fakePublisher) OnConnectionUp(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upFns = append(p.upFns, fn)
}

func (p *fakePublisher) Shutdown(_ context.Context, statusTopic string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.stopTopic = statusTopic
	return nil
}

// connect runs the registered connection-up callbacks, as autopaho would.
func (p *fakePublisher) connect() {
	p.mu.Lock()
	fns := slices.Clone(p.upFns)
	p.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// deliver hands a message to the subscription registered for topic.
func (p *fakePublisher) deliver(t *testing.T, topic string, payload []byte) {
	t.Helper()
	p.mu.Lock()
	cb := p.subs[topic]
	p.mu.Unlock()
	if cb == nil {
		t.Fatalf("nothing is subscribed to %q", topic)
	}
	cb(topic, payload)
}

func (p *fakePublisher) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failure = err
}

func (p *fakePublisher) observed() []pubMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pubMsg(nil), p.msgs...)
}

// payloads returns every payload published to topic, in order.
func (p *fakePublisher) payloads(topic string) []string {
	var out []string
	for _, m := range p.observed() {
		if m.topic == topic {
			out = append(out, m.payload)
		}
	}
	return out
}

// last returns the most recent payload published to topic.
func (p *fakePublisher) last(topic string) (string, bool) {
	got := p.payloads(topic)
	if len(got) == 0 {
		return "", false
	}
	return got[len(got)-1], true
}

// firstIndex returns the position of the first publish to topic, or -1.
func (p *fakePublisher) firstIndex(topic string) int {
	for i, m := range p.observed() {
		if m.topic == topic {
			return i
		}
	}
	return -1
}

// indexOf returns the position of the first publish of payload to topic, or -1.
func (p *fakePublisher) indexOf(topic, payload string) int {
	for i, m := range p.observed() {
		if m.topic == topic && m.payload == payload {
			return i
		}
	}
	return -1
}

func (p *fakePublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = nil
}

func formatMsgs(msgs []pubMsg) string {
	if len(msgs) == 0 {
		return "(nothing)"
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.String()
	}
	return strings.Join(parts, "\n  ")
}

// ------------------------------------------------------------- notifier ----

type fakeNotifier struct {
	mu sync.Mutex

	interval time.Duration
	armed    bool

	ready    int
	stopping int
	pings    int
	statuses []string

	// pingErr is what Watchdog returns, so a test can hold the notification
	// socket down across many ticks.
	pingErr error
}

func (n *fakeNotifier) setPingErr(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pingErr = err
}

func (n *fakeNotifier) Ready() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ready++
	return nil
}

func (n *fakeNotifier) Stopping() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopping++
	return nil
}

func (n *fakeNotifier) Watchdog() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pings++
	return n.pingErr
}

func (n *fakeNotifier) Status(s string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.statuses = append(n.statuses, s)
	return nil
}

func (n *fakeNotifier) WatchdogInterval() (time.Duration, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.interval, n.armed
}

func (n *fakeNotifier) pingCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pings
}

func (n *fakeNotifier) lastStatus() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.statuses) == 0 {
		return ""
	}
	return n.statuses[len(n.statuses)-1]
}

// ---------------------------------------------------------------- zones ----

type fakeZones struct {
	mu        sync.Mutex
	state     string
	loc       *time.Location
	applied   []string
	rejectAll bool
}

func (z *fakeZones) Apply(value string) string {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.applied = append(z.applied, value)
	if z.rejectAll {
		return z.state // re-echo what is in force, as the real manager does
	}
	z.state = value
	return value
}

func (z *fakeZones) State() string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.state
}

func (z *fakeZones) Location() *time.Location {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.loc
}

func (z *fakeZones) writes() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.applied...)
}

// ----------------------------------------------------------- integrator ----

type fakeIntegrator struct {
	mu       sync.Mutex
	samples  []dliSample
	reading  dli.Reading
	zones    []string
	flushed  int
	flushErr error
}

type dliSample struct {
	at   time.Time
	ppfd float64
}

func (i *fakeIntegrator) Add(t time.Time, ppfd float64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.samples = append(i.samples, dliSample{at: t, ppfd: ppfd})
}

func (i *fakeIntegrator) SetLocation(loc *time.Location, zone string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.zones = append(i.zones, zone+"|"+loc.String())
}

func (i *fakeIntegrator) Snapshot() dli.Reading {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reading
}

func (i *fakeIntegrator) Flush() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.flushed++
	return i.flushErr
}

func (i *fakeIntegrator) added() []dliSample {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]dliSample(nil), i.samples...)
}

func (i *fakeIntegrator) set(r dli.Reading) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reading = r
}

func (i *fakeIntegrator) zonesSet() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.zones...)
}

func (i *fakeIntegrator) flushes() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.flushed
}

// --------------------------------------------------------------- errors ----

// Wrapped sentinels, so a test asserts on the classification rather than on an
// error value the production code could never see.
func wrapped(sentinel error, what string) error {
	return fmt.Errorf("sdi12: 0M!: %s: %w", what, sentinel)
}

var (
	errPortDead   = wrapped(sdi12.ErrPortDead, "write: input/output error")
	errProtocol   = wrapped(sdi12.ErrProtocol, "measurement header %!q(MISSING)")
	errNoResponse = wrapped(sdi12.ErrNoResponse, "read timed out")
	// Both ErrHeaderNoValues paths, exactly as sdi12 produces them.
	errHeaderZero = fmt.Errorf("sdi12: 0M4!: measurement reports 0 values: %w: %w",
		sdi12.ErrHeaderNoValues, sdi12.ErrNoValues)
	// The 0V! shape of the same thing. It needs its own value because a
	// definitive "I have no coefficient" is the one answer that must retract
	// cal_factor, and Verify reports it as an error rather than as (nil, nil) —
	// a fake that returned (nil, nil) would exercise a branch production cannot
	// reach and leave the real one untested, which is exactly what happened.
	errVerifyHeaderZero = fmt.Errorf("sdi12: 0V!: measurement reports 0 values: %w: %w",
		sdi12.ErrHeaderNoValues, sdi12.ErrNoValues)
	errDataMissing = wrapped(sdi12.ErrNoValues, "0D0!: data response carried no values")
)

// --------------------------------------------------------------- logger ----

// testLogger routes package logging into t.Log, with a guard against writing
// after the test finishes: the poll goroutine can outlive a cancelled test by
// microseconds and a stray t.Log then panics the whole run.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	w := &testWriter{t: t}
	t.Cleanup(w.done)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logSink captures what the daemon would write to the journal, at the level the
// process actually runs at.
//
// Several of the defects this package guards against are log-volume defects: a
// warning that repeats once per port session is thousands of lines a day for
// one unplugged wire, and the flood buries the one line that says when the
// trouble started. That makes "how many times does this line appear" a property
// worth a test, exactly like a payload.
//
// The level is Info rather than Debug because that is what cmd/apogee-sq521
// pins its handler to; a line demoted to Debug is a line an operator never
// sees, which is the whole point of demoting it.
type logSink struct {
	mu    sync.Mutex
	t     *testing.T
	lines []string
}

func newLogSink(t *testing.T) *logSink {
	t.Helper()
	return &logSink{t: t}
}

func (s *logSink) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (s *logSink) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	return len(p), nil
}

// count returns how many captured lines contain substr.
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

// matching returns the captured lines containing substr.
func (s *logSink) matching(substr string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, line := range s.lines {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// dump returns everything captured, for a failure message.
func (s *logSink) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return "(nothing)"
	}
	return "  " + strings.Join(s.lines, "\n  ")
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

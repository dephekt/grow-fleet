// Package app is the integration layer. It wires the daemon's library packages
// into a running process and owns the orchestration none of them can own alone:
// what happens in what order, on which goroutine, and how a failure is
// classified.
//
// It contains no new primitives. Framing is sdi12's, the fd is serialport's,
// the will and its reason code are mqttpub's, the entity table is entities',
// integration is dli's, and the TZ parser is timezone's. What lives here is the
// glue those packages document but cannot enforce.
//
// # The orderings this package exists to enforce
//
// Four of them, each corresponding to a defect measured in the Python publisher
// this replaces:
//
//  1. The capability probe completes before discovery is published.
//     entities.Snapshot documents the contract and cannot enforce it: a zero
//     Snapshot is indistinguishable from "nothing has been asked", so publishing
//     discovery at MQTT connect — as the Python did in on_connect — announces
//     tilt on a pre-3033 unit and the first poll then marks it unsupported,
//     leaving a permanently valueless entity with a retained config. Here,
//     onConnectionUp publishes nothing until the probe has run; see
//     App.publishBirth.
//
//  2. entities.EligibleSet is the single funnel for discovery *and* polling.
//     Two derivations of "which entities are live" would drift, which is what
//     the Python's two parallel tables did.
//
//  3. MQTT starts before the serial port is touched. The Python retried the
//     serial open for up to 36 s *before* configuring MQTT, so during the window
//     where the daemon was most likely to die no will was registered. Here
//     Publisher.Start is non-blocking and runs first; the open lives in the poll
//     goroutine behind a context-aware backoff.
//
//  4. A retained discovery config is retracted when its entity stops being
//     eligible. Retained messages do not clear themselves, so a probe that flips
//     tilt to unsupported on a later port session must publish an empty retained
//     payload to the config topic or the entity outlives the daemon's opinion of
//     it forever.
//
//     Ineligibility is asserted against the broker rather than diffed against
//     what this process remembers publishing. The common case is a *restart* —
//     systemd, a deploy, a reboot, a sensor swap — where the retained config
//     belongs to a previous process and this one has published nothing at all;
//     a diff against our own memory would leave it standing forever, which is
//     the same permanently valueless entity constraint 4 exists to prevent. See
//     App.retract.
//
//     A transient failure is never grounds for a retraction. A 0I!/0V! timeout
//     on the reopen a flaky sensor forces would otherwise delete two established
//     entities, and reopens happen exactly when a timeout is most likely; see
//     probeResult.inconclusive.
//
// # Goroutine topology
//
// The process runs four goroutines of its own, plus autopaho's internals:
//
//	Run          registers callbacks, starts MQTT, waits for ctx, tears down
//	 ├─ poller   SOLE owner of the serial port: open, probe, cycle, reopen
//	 ├─ health   watchdog pinger and status-line refresher; both are no-ops
//	 │           outside systemd, so the loop runs unconditionally
//	 └─ timezone drains reconciler writes off autopaho's network goroutine
//
// The timezone goroutine is the one addition to the obvious three, and it is
// forced: mqttpub delivers an inbound PUBLISH on the network goroutine that
// received it and documents that the callback must not block, while applying a
// zone writes a file and echoing it publishes with a two-second budget. The
// callback therefore only enqueues.
//
// Invariants:
//
//   - Exactly one goroutine owns the serial port, so there is no mutex around
//     it, no concurrent-close hazard, and no question of who called Close while
//     a Read was pending.
//   - Exactly one goroutine publishes at a time. App.sequence holds seqMu across
//     a whole run of publishes — a birth, a cycle's readings, a timezone echo —
//     so a reconnection birth on autopaho's goroutine cannot interleave with the
//     poller's own publishes. Without it a retained value can land ahead of the
//     discovery config that resolves it, and the publish-then-record pairs can
//     leave "online" retained while App.online records false, which is this
//     project's headline failure mode.
//   - Every blocking operation is bounded. Serial I/O by serialport's deadlines,
//     MQTT publishes by a context timeout, and every wait by a select on
//     ctx.Done(). There is no time.Sleep in the binary.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// The device block, byte-identical to the Python's DEVICE dict. grow-app groups
// entities into one device card by these strings, and grow-history-recorder
// tags InfluxDB series with the node id, so they are wire contract.
const (
	deviceName         = "Quantum PAR Sensor"
	deviceManufacturer = "Apogee Instruments"
	deviceModel        = "SQ-521"
)

// publishTimeout bounds one MQTT publish.
//
// mqttpub.PublishRetained returns immediately when the link is down rather than
// queueing, so this only ever bounds a publish that is genuinely in flight to a
// reachable broker on the same LAN segment. Two seconds is far past any healthy
// round trip and short enough that a stalled TCP connection cannot hold up a
// poll cycle.
const publishTimeout = 2 * time.Second

// Mirrors of two unexported constants in internal/sdi12, needed to size the
// per-cycle budget. They are duplicated rather than exported because they are
// implementation details of that package's transaction; TestTransactionBudget
// documents what they buy, and a change there that this does not follow makes
// the budget conservative rather than wrong.
const (
	// sdi12ServiceRequestSlack is the second sdi12 adds to the sensor's own ttt
	// when waiting for the data-ready notification (sdi12/client.go,
	// serviceRequestSlack).
	sdi12ServiceRequestSlack = time.Second
	// sdi12MaxDataCommands is how many D-commands one collection may walk
	// (sdi12/client.go, maxDataCommands).
	sdi12MaxDataCommands = 3
)

// maxConversionWait is the largest ttt this daemon sizes its budget for.
//
// THE TRAP this closes: sdi12 clamps every read to the caller's context
// deadline, and it deliberately refuses to send aD0! when that clamp cut the
// conversion wait short — proceeding would put a data command on the bus while
// the measurement is still running, which is the desync the package exists to
// eliminate. So a per-cycle context sized from POLL_SECONDS rather than from the
// sensor's declared ttt does not merely run late, it fails *every* cycle with
// context.DeadlineExceeded. Measured against a ttt of 001: a 1.9 s budget
// returns DeadlineExceeded, a 2.1 s budget returns the value.
//
// The SQ-521 declares 001 for 0M!, 0M1! and 0M4!. Two seconds is double that,
// so a unit or a firmware that answered 002 still fits. A sensor declaring more
// than this is not silently mis-served: sdi12 refuses the aD0! and the failure
// is logged with the shortfall, which is the signal to raise this number.
const maxConversionWait = 2 * time.Second

// The poller's reopen and backoff policy.
const (
	// reopenAfterSoftFailures escalates a run of transient failures — malformed
	// frames and silence, neither of which implicates the descriptor — into a
	// close-and-reopen. Three cycles of a sensor that is answering nothing
	// usable is no longer transient, and the adapter's MCU reset is the only
	// remaining lever.
	reopenAfterSoftFailures = 3

	// dialBackoffMin and dialBackoffMax bound the retry gap when the device
	// cannot be opened. USB enumeration routinely lags a Pi reboot by seconds,
	// so the first retry is quick; a genuinely absent adapter settles at the
	// ceiling rather than spinning on the bus.
	dialBackoffMin = time.Second
	dialBackoffMax = 30 * time.Second
)

// Deps are the collaborators [Run] needs. Production fills them from the real
// packages; tests substitute fakes. Every field is required.
type Deps struct {
	Dialer     Dialer
	Publisher  Publisher
	Notifier   Notifier
	Zones      Zones
	Integrator Integrator

	// Rollovers is the sink handed to dli.Config.OnRollover at construction.
	// See RolloverSink for why the events cannot be published from the handler.
	Rollovers *RolloverSink

	// Clock is optional; nil selects the real one.
	Clock Clock
	// Logger is optional; nil discards.
	Logger *slog.Logger
}

// RolloverSink buffers dli rollover events until the poll goroutine can publish
// them.
//
// dli calls Config.OnRollover synchronously from Add, with its own lock held,
// and documents that the handler must neither re-enter the accumulator nor
// block. Publishing does both things it must not do — it takes up to
// publishTimeout, and the reading it would want is the one already handed to
// it. So the handler only appends, and the poll goroutine drains the sink on
// its own stack immediately after the Add that produced the events.
//
// A sink is safe for concurrent use, which costs nothing and means a future
// second producer cannot introduce a race here silently.
type RolloverSink struct {
	mu     sync.Mutex
	events []dli.Event
}

// Handle records one event. Assign it to dli.Config.OnRollover.
func (s *RolloverSink) Handle(e dli.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// Drain returns and clears the buffered events, oldest first.
func (s *RolloverSink) Drain() []dli.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	out := s.events
	s.events = nil
	return out
}

// App is one configured daemon.
type App struct {
	cfg  config.Config
	deps Deps

	log   *slog.Logger
	clock Clock

	topics entities.Topics
	// table is the full entity table, rebuilt never: EligibleSet filters a copy
	// so the table itself survives every probe.
	table []entities.Entity
	aux   []auxEntity
	// device is the shared block minus sw_version, which arrives from the probe.
	device entities.Device

	// cycleBudget is the ceiling for one poll cycle, and one of the spans the
	// watchdog's staleness tolerance is built from. See maxConversionWait.
	cycleBudget time.Duration
	// birthBudget is the ceiling for one whole birth sequence. See birthBudget.
	birthBudget time.Duration

	// heartbeat is the Unix-nanosecond stamp of the poll goroutine's last
	// iteration. It means "the poll goroutine is scheduling and making
	// progress", NOT "the sensor is answering" — see runHealth.
	heartbeat atomic.Int64

	// tzCmd carries reconciler writes from autopaho's network goroutine to the
	// timezone goroutine. Capacity 1; see onTimezoneCommand for why a full
	// channel drops rather than blocks.
	tzCmd chan string

	// seqMu serialises whole runs of publishes against each other: a birth, a
	// cycle's readings, a timezone echo. Three goroutines can start one — the
	// poller, autopaho's connection-up callback, the timezone goroutine — and
	// interleaving them would put a discovery config after the "online" it is
	// supposed to precede, or a retained value ahead of the config that
	// resolves it. See App.sequence.
	seqMu sync.Mutex

	mu sync.Mutex // guards the fields below
	// probed is the gate for constraint 1: false means no Snapshot exists yet
	// and no discovery may be published.
	probed   bool
	snapshot entities.Snapshot
	working  []entities.Entity
	// inconclusive is the last probe's set of object ids whose step *errored*,
	// as opposed to answering that the sensor has no such value. Nothing may be
	// concluded about them, and in particular they may not be retracted: see
	// probeResult.inconclusive and App.retract.
	//
	// It is replaced wholesale by adopt rather than mutated, so the copy a birth
	// on another goroutine is walking stays valid.
	inconclusive map[string]bool
	// retained maps object id to the discovery payload this process believes is
	// on the broker, the empty string meaning "we published the retraction". It
	// is what makes a birth idempotent — a mute sensor re-births every
	// reopenAfterSoftFailures cycles and must not republish an unchanged config
	// each time — and it is cleared by forgetRetained on every connection-up.
	retained map[string]string
	// lastValues maps object id to the payload last published to its state
	// topic, including the empty string a failed cycle publishes.
	lastValues map[string]string
	// lastReadingAt is when PPFD last reached its state topic with a real
	// value, so statusLine can say how old the number it prints is.
	lastReadingAt time.Time
	// online is the availability last published, so a transition can be
	// detected and logged once instead of every cycle. availPublished
	// distinguishes "we have published offline" from "we have published
	// nothing"; see setAvailability.
	online         bool
	availPublished bool
	// degraded is the failure ladder's verdict — "the sensor is not answering"
	// — as opposed to online, which is what was last put on the status topic.
	// See degradedNow.
	degraded bool
	// out describes why the ladder last declared an outage, so the line that
	// marks it carries something an operator can act on.
	out outage
	// publishFailing is the same transition guard for the broker link, so an
	// outage costs one log line rather than one per publish per cycle. It is
	// also the only evidence the daemon has that the link is up, which is what
	// statusLine reports: nothing else observes the connection. publishSeen
	// distinguishes "every publish is failing" from "nothing has been tried",
	// and lastPublishAt is when one last succeeded.
	publishFailing bool
	publishSeen    bool
	lastPublishAt  time.Time
}

// ErrStartup marks a [App.Run] failure that happened before the daemon was
// running, as opposed to one that happened on the way out.
//
// The two need different exit codes and Run returns them through the same
// error. A teardown that did not complete is not worth a non-zero exit — the
// broker's will covers the retained availability — but a start that failed is:
// without the distinction a daemon that could not bring MQTT up at all exits 0,
// systemd's Restart=on-failure declines to restart it, and the unit sits
// "inactive (dead)" with nothing but a warning in the journal. That is exactly
// the silent-success failure mode this codebase exists to eliminate.
var ErrStartup = errors.New("app: could not start")

// outage is what the failure ladder knows about the current run of failures.
//
// It exists because the line that marks an outage used to name only an MQTT
// topic, and in a live run it was the last thing in the journal before an hour
// of silence — the actionable detail was in the previous line, which ages out
// of `journalctl -n`. Everything needed to act is now on the one line that
// matters.
type outage struct {
	// reason is the human-readable classification, e.g. "serial open failed".
	reason string
	// err is the error that classified it.
	err error
	// attempts is how many consecutive attempts had failed at that point.
	attempts int
	// since is when the run of failures started.
	since time.Time
}

// New validates cfg's collaborators and returns an App that has not started.
func New(cfg config.Config, deps Deps) (*App, error) {
	var missing []error
	if deps.Dialer == nil {
		missing = append(missing, errors.New("Dialer is required"))
	}
	if deps.Publisher == nil {
		missing = append(missing, errors.New("Publisher is required"))
	}
	if deps.Notifier == nil {
		missing = append(missing, errors.New("Notifier is required"))
	}
	if deps.Zones == nil {
		missing = append(missing, errors.New("Zones is required"))
	}
	if deps.Integrator == nil {
		missing = append(missing, errors.New("Integrator is required"))
	}
	if deps.Rollovers == nil {
		missing = append(missing, errors.New("Rollovers is required"))
	}
	if err := errors.Join(missing...); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	if deps.Clock == nil {
		deps.Clock = realClock{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}

	table := entities.Build(entities.Params{PPFDUnit: cfg.PPFDUnit})
	aux := auxTable(cfg.PPFDUnit)

	return &App{
		cfg:    cfg,
		deps:   deps,
		log:    deps.Logger,
		clock:  deps.Clock,
		topics: entities.Topics{Prefix: cfg.TopicPrefix, NodeID: cfg.NodeID},
		table:  table,
		aux:    aux,
		device: entities.Device{
			Identifiers:  []string{cfg.NodeID},
			Name:         deviceName,
			Manufacturer: deviceManufacturer,
			Model:        deviceModel,
		},
		// Sized from the whole table rather than from whatever the probe leaves
		// eligible, so the budget — and the watchdog tolerance derived from it —
		// does not change under the watchdog when tilt is retired mid-run.
		cycleBudget: cycleBudget(table, cfg.ReadTimeout),
		birthBudget: birthBudget(table, aux),
		tzCmd:       make(chan string, 1),
		retained:    map[string]string{},
		lastValues:  map[string]string{},
	}, nil
}

// transactionBudget is the wall-clock ceiling for one aM<sub>! transaction,
// summed from the four waits sdi12 can make inside one:
//
//	readTimeout                             the "atttn" header
//	maxConversionWait + serviceRequestSlack the data-ready notification
//	maxDataCommands * readTimeout           the D0!/D1!/D2! walk
//
// It is a ceiling, not a schedule: a healthy transaction completes in
// milliseconds. Its only jobs are to stop a wedged adapter from holding a cycle
// open indefinitely, and to stay comfortably above ttt + 1 s so sdi12 never
// refuses the aD0! for want of budget.
func transactionBudget(readTimeout time.Duration) time.Duration {
	return readTimeout +
		maxConversionWait + sdi12ServiceRequestSlack +
		sdi12MaxDataCommands*readTimeout
}

// cycleBudget is one transaction budget per distinct measurement sub-command in
// the table. Entities sharing a sub-command share its transaction, so the
// budget follows the number of transactions rather than the number of entities.
func cycleBudget(table []entities.Entity, readTimeout time.Duration) time.Duration {
	groups := groupBySubCmd(table)
	n := len(groups)
	if n == 0 {
		n = 1
	}
	return time.Duration(n) * transactionBudget(readTimeout)
}

// birthBudget is the wall-clock ceiling for one whole birth sequence: every
// retained message this daemon owns, each taking the full publishTimeout.
//
// It is not a schedule either — a birth against a healthy broker on the same
// LAN segment is milliseconds. It exists because a birth holds seqMu, and a
// reconnection births on autopaho's goroutine, so this is time the poll
// goroutine can legitimately spend blocked and not stamping the heartbeat.
// stalenessTolerance therefore carries it; without it, a slow broker during a
// reconnection is indistinguishable from a wedged poll loop and systemd
// restarts the daemon, re-enumerating the USB adapter for nothing.
//
// One config or one retraction per entity, one state topic per entity, plus the
// availability and the timezone echo.
func birthBudget(table []entities.Entity, aux []auxEntity) time.Duration {
	return time.Duration(2*(len(table)+len(aux))+2) * publishTimeout
}

// offlineAfterAtMost is the longest a sensor can stop answering before the
// retained availability turns "offline".
//
// OFFLINE_AFTER counts *cycles*, not seconds, and a failing cycle does not
// finish promptly — a silent sensor spends the whole cycle budget waiting for
// it. The wait between boundaries is absorbed rather than added, because a
// cycle that overran skips boundaries instead of catching up, so the worst case
// is OFFLINE_AFTER cycles each taking max(interval, budget).
//
// It is reported at startup and in --check-config because the number an
// operator computes from OFFLINE_AFTER × POLL_SECONDS is the wrong one, and
// waiting 30 s for an outage that takes 99 s to appear is how a working daemon
// gets restarted.
func (a *App) offlineAfterAtMost() time.Duration {
	return time.Duration(a.cfg.OfflineAfter) * max(a.cfg.PollInterval, a.cycleBudget)
}

// pollerCtxKey marks a context as belonging to the poll goroutine.
//
// The heartbeat means "the poll goroutine is scheduling and making progress",
// so only that goroutine may stamp it — and which goroutine a publish is
// running on is not otherwise knowable inside App.publish, which is reached
// both from the poll loop and from autopaho's connection-up callback. A
// heartbeat stamped by a reconnection would keep the watchdog fed while the
// poll loop was wedged, which is the one failure the watchdog exists to catch.
//
// runPoller marks its context once. Every context derived from it — the
// per-cycle budget, the per-publish timeout — carries the mark; Run's context,
// which the reconnection birth and the timezone goroutine use, does not.
type pollerCtxKey struct{}

func withPoller(ctx context.Context) context.Context {
	return context.WithValue(ctx, pollerCtxKey{}, true)
}

func onPoller(ctx context.Context) bool {
	marked, _ := ctx.Value(pollerCtxKey{}).(bool)
	return marked
}

// sequence runs fn as one uninterrupted run of publishes.
//
// Every publish path in this package goes through it. The orderings this
// package exists to enforce — discovery before availability, availability
// before values — are properties of a *sequence*, and two sequences on two
// goroutines interleaving destroys them just as thoroughly as publishing them
// in the wrong order would. It also makes each publish-then-record pair atomic
// against the other publishers, so the broker cannot retain "online" while
// App.online records false: setAvailability only publishes on change, so that
// divergence is never corrected.
func (a *App) sequence(fn func()) {
	a.seqMu.Lock()
	defer a.seqMu.Unlock()
	fn()
}

// Run starts the daemon and blocks until ctx is done, then tears it down.
//
// The teardown order is the part that matters, and it is the reverse of what
// the Python did: our goroutines stop first, and only then does mqttpub publish
// the retained "offline" and choose its DISCONNECT reason code. A publish
// racing that sequence could leave "online" retained for a process that has
// exited, which grow-app renders as a sensor reporting a constant value rather
// than as an outage.
func (a *App) Run(ctx context.Context) error {
	// Registered before Start so the very first connection is covered. The
	// callback closes over ctx rather than reading a field, which keeps the
	// happens-before obvious: nothing can invoke it before Start returns.
	a.deps.Publisher.OnConnectionUp(func() { a.publishBirth(ctx, birthReconnected) })

	// Subscribing before Start is deliberate and not an error: mqttpub records
	// the subscription and issues the SUBSCRIBE from every connection-up
	// sequence, ahead of the callbacks above, so the command topic is live
	// before the first discovery config is announced.
	if err := a.deps.Publisher.Subscribe(ctx, a.timeZoneCommandTopic(), a.onTimezoneCommand); err != nil {
		a.log.Warn("could not subscribe to the timezone command topic; the next reconnection will retry",
			"topic", a.timeZoneCommandTopic(), "error", err)
	}

	if err := a.deps.Publisher.Start(ctx); err != nil {
		return fmt.Errorf("%w: start mqtt: %w", ErrStartup, err)
	}

	// Ready as soon as MQTT is dialing, not after the sensor opens. A Type=notify
	// unit that withheld READY=1 until the adapter enumerated would sit in
	// "activating" through exactly the outage an operator most wants to see, and
	// would eventually be killed by TimeoutStartSec for a fault the daemon is
	// designed to survive.
	if err := a.deps.Notifier.Ready(); err != nil {
		a.log.Warn("could not send READY=1 to the service manager", "error", err)
	}
	// offline_after is spelled out in seconds as well as in cycles because the
	// two are not the same number and the difference is not guessable: a cycle
	// may spend a whole cycle_budget before it counts as failed, so
	// OFFLINE_AFTER=3 at a 10 s cadence is "up to 99 s", not 30. An operator who
	// computes the wrong one concludes the daemon is wedged while it is working
	// exactly as configured. --check-config says the same thing at more length.
	a.log.Info("started",
		"node_id", a.cfg.NodeID,
		"broker", fmt.Sprintf("%s:%d", a.cfg.MQTTHost, a.cfg.MQTTPort),
		"device", a.cfg.SDI12Port,
		"poll_interval", a.cfg.PollInterval,
		"cycle_budget", a.cycleBudget,
		"offline_after_cycles", a.cfg.OfflineAfter,
		"offline_after_at_most", a.offlineAfterAtMost(),
	)

	// Seeded before the health goroutine can read it. The heartbeat's zero value
	// is the Unix epoch, so a health tick that lands before the poll goroutine's
	// very first beat computes a staleness of five decades, logs the Error that
	// says the loop has parked, and withholds the watchdog ping — about a daemon
	// that is a few microseconds old.
	a.beat()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.runPoller(ctx) }()
	go func() { defer wg.Done(); a.runHealth(ctx) }()
	go func() { defer wg.Done(); a.runTimezone(ctx) }()

	<-ctx.Done()
	// STOPPING=1 before the drain, so systemd stops counting the unit as live
	// and the watchdog cannot trip while we are on the way out.
	if err := a.deps.Notifier.Stopping(); err != nil {
		a.log.Warn("could not send STOPPING=1 to the service manager", "error", err)
	}
	a.log.Info("shutting down")
	wg.Wait()

	var errs []error
	// mqttpub owns the WithoutCancel and the 0x04 reason code; ctx is already
	// cancelled here and passing it is correct — Shutdown uses it for values
	// only.
	if err := a.deps.Publisher.Shutdown(ctx, a.topics.Status()); err != nil {
		errs = append(errs, err)
	}
	// Immediately after, not before: the coalescing cadence otherwise leaves up
	// to a minute of integration unwritten, and this is the one place a clean
	// stop can make that up.
	if err := a.deps.Integrator.Flush(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Package mqttpub wraps autopaho with the one piece of behaviour this daemon
// cannot afford to get wrong: the retained availability status must never be
// left saying "online" for a process that has exited.
//
// grow-app treats "<base>/status" == "online" as permission to render the last
// retained reading as live. A stale "online" therefore does not look like an
// outage — it looks like a sensor reporting a constant value, which is exactly
// what a PAR sensor does at night. The failure is silent for as long as nobody
// happens to compare the timestamp.
//
// The Python publisher this replaces got the teardown backwards:
//
//	publish(offline) -> sleep(0.3) -> loop_stop() -> disconnect()
//
// loop_stop() kills the network thread with the PUBLISH possibly still queued,
// and the disconnect() that follows is a *clean* one, which per MQTT-3.14.4-3
// tells the broker to discard the will. Both deliveries of "offline" could be
// lost from the same shutdown, and the retained status stayed "online" forever.
//
// # The rule
//
// A clean DISCONNECT is permitted only when the "offline" PUBACK is in hand.
//
// The abort mechanism is MQTT v5 reason code 0x04, "Disconnect with Will
// Message": a protocol-correct disconnect that still asks the broker to publish
// the will. It is selected by [Publisher.disconnectPacket] from a single
// atomic.Bool, so it covers *every* teardown path rather than one branch — an
// abandoned context, a failed publish and a panicking caller all end the same
// way.
//
// These are the measured results against autopaho v0.23.0 and an embedded
// broker; each row is what the broker observed, not what the client returned:
//
//	teardown                                          will
//	-------------------------------------------------------------
//	cancel the connection-manager ctx, no Disconnect   suppressed
//	DisconnectPacketBuilder returning nil              suppressed
//	explicit Disconnect()                              discarded
//	DisconnectPacketBuilder returning ReasonCode 0x04  fires
//
// The first two rows are the trap: cancelling the context looks like an abort,
// but autopaho reacts to a cancelled context by sending a clean DISCONNECT of
// its own (autopaho/auto.go:341-348), and returning nil from the builder skips
// our packet without skipping its close. Neither is a way to keep the will.
//
// # Publishing
//
// State is published with [ConnectionManager.Publish], which returns
// "connection with the MQTT server is currently down" rather than queueing.
// That is deliberate: PublishViaQueue would replay a reading recorded during an
// outage once the link returned, and a replayed reading arrives on a retained
// state topic indistinguishable from a fresh one. A missing reading is
// recoverable; a stale one presented as live is not.
package mqttpub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
	"github.com/eclipse/paho.golang/paho/log"
	"github.com/eclipse/paho.golang/paho/session"
)

// Availability payloads. grow-app compares the retained status against these
// exact strings, so they are wire contract rather than presentation.
const (
	PayloadOnline  = "online"
	PayloadOffline = "offline"
)

const (
	// clientIDPrefix names the daemon on the broker. The full id is
	// "apogee-<nodeID>" and is deliberately *stable* across restarts.
	//
	// A random id would be worse in the case that matters. After an ungraceful
	// kill the broker still holds the orphaned session; when we come back with
	// the same id the broker's takeover disconnect of that session fires its
	// will before our CONNACK, and the birth "online" published from
	// OnConnectionUp immediately overwrites it. With a random id the orphan
	// lingers until keepalive expiry, and the "offline" it eventually emits
	// lands *after* our birth message, leaving a live daemon marked offline.
	//
	// The usual objection to a stable id — two copies fighting over it — is
	// handled upstream by the instance lock: a second copy never reaches MQTT.
	clientIDPrefix = "apogee-"

	// defaultKeepAlive halves the Python's 30 s. The broker declares a client
	// dead after 1.5 keepalives, so this bounds worst-case will latency to ~15 s
	// on a wedged link (a yanked USB hub, a Pi that lost its network) where
	// there is no FIN to notice.
	defaultKeepAlive = 10

	// defaultConnectRetryDelay is the gap between connection attempts. The
	// broker and this daemon sit on the same LAN segment; 5 s recovers from a
	// broker restart quickly without hammering it during a long outage.
	defaultConnectRetryDelay = 5 * time.Second

	// defaultConnectTimeout matches autopaho's own default and also bounds the
	// re-subscribe issued from the connection-up sequence.
	defaultConnectTimeout = 10 * time.Second

	// defaultShutdownTimeout bounds the whole of Shutdown when the caller's
	// Config leaves it unset. config.Config validates the real value.
	defaultShutdownTimeout = 3 * time.Second

	// MQTT v5 DISCONNECT reason codes. 0x00 is "Normal disconnection", which
	// instructs the broker to discard the will; 0x04 is "Disconnect with Will
	// Message", which is equally clean but asks for the will to be published.
	disconnectNormal   = 0x00
	disconnectWithWill = 0x04

	// teardownShare is the fraction of ShutdownTimeout reserved for each of the
	// two phases that are not the "offline" publish: letting an in-flight birth
	// sequence finish, and getting the DISCONNECT written. The publish gets what
	// is left (half the budget in the worst case, all of it in practice, since
	// both reserves are normally consumed in microseconds).
	//
	// The reserves exist because a teardown that overruns its deadline leaves
	// the broker seeing a dropped socket instead of our 0x04 packet. That still
	// produces an "offline" — the will fires on an abnormal close too — but it
	// arrives one keepalive later rather than immediately.
	teardownShare = 4
)

// Errors returned by Publisher. They are sentinels because the daemon logs
// shutdown failures differently from publish failures: an unconfirmed "offline"
// means "the will is now load-bearing", which is worth a distinct log line.
var (
	// ErrNotStarted is returned by operations that need a connection manager
	// before Start has created one.
	ErrNotStarted = errors.New("mqttpub: publisher not started")

	// ErrAlreadyStarted is returned by a second call to Start.
	ErrAlreadyStarted = errors.New("mqttpub: publisher already started")

	// ErrShutdown is returned by publishes attempted after Shutdown has begun.
	// Shutdown's own "offline" publish bypasses this check.
	ErrShutdown = errors.New("mqttpub: publisher is shutting down")

	// ErrStatusTopicMismatch is returned when Shutdown is asked to publish
	// "offline" somewhere other than the topic the will was registered on. See
	// Shutdown for why that is refused rather than obeyed.
	ErrStatusTopicMismatch = errors.New("mqttpub: shutdown status topic does not match the will topic")
)

// Config describes the broker connection. Zero values for the tunables select
// the package defaults, so a caller only has to fill in the four fields that
// have no sensible default.
type Config struct {
	// Host and Port address the broker.
	Host string
	Port int

	// Username and Password authenticate to the broker. Both empty is legal
	// here (an anonymous broker is a valid MQTT server); config.Config is where
	// this daemon refuses to run without credentials.
	Username string
	Password string

	// NodeID is the device identity. It only appears here as the client id
	// suffix; topics are built by the entities package.
	NodeID string

	// StatusTopic is the availability topic — entities.Topics.Status(). It is
	// registered as the will at CONNECT, so it must be known before the first
	// connection rather than at shutdown. This field is the only thing that
	// decides where "offline" goes: Shutdown takes the same string as an
	// argument but treats it as an assertion to check, not as a choice.
	StatusTopic string

	// ShutdownTimeout bounds all of Shutdown. Defaults to 3s.
	ShutdownTimeout time.Duration

	// KeepAlive, in seconds, defaults to 10.
	KeepAlive uint16

	// ConnectRetryDelay is the gap between connection attempts. Defaults to 5s.
	ConnectRetryDelay time.Duration

	// ConnectTimeout bounds one connection attempt. Defaults to 10s.
	ConnectTimeout time.Duration

	// Logger receives connection-lifecycle events. Nil discards them.
	//
	// Connection errors are logged rather than returned because Start is
	// non-blocking: from the caller's point of view an unreachable broker is not
	// an error, it is a state the daemon runs in until it changes. Silence there
	// is what made the Python's permanently-failing CONNECT invisible.
	Logger *slog.Logger
}

// withDefaults returns cfg with zero-valued tunables filled in.
func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.KeepAlive == 0 {
		c.KeepAlive = defaultKeepAlive
	}
	if c.ConnectRetryDelay <= 0 {
		c.ConnectRetryDelay = defaultConnectRetryDelay
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	return c
}

// validate reports every problem at once, matching the config package's habit
// of not making the operator fix one field per restart.
func (c Config) validate() error {
	var errs []error
	if c.Host == "" {
		errs = append(errs, errors.New("Host must not be empty"))
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("Port %d: must be a TCP port in 1..65535", c.Port))
	}
	if c.NodeID == "" {
		errs = append(errs, errors.New("NodeID must not be empty (it is the client id suffix)"))
	}
	if c.StatusTopic == "" {
		errs = append(errs, errors.New("StatusTopic must not be empty (it is the will topic)"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("mqttpub: invalid config: %w", err)
	}
	return nil
}

// ClientID returns the stable MQTT client id for a node. See clientIDPrefix for
// why it is stable rather than random.
func ClientID(nodeID string) string { return clientIDPrefix + nodeID }

// subscription pairs a topic filter with the callback that wants it. Both are
// retained for the lifetime of the Publisher so that every reconnection can
// re-issue the SUBSCRIBE; MQTT subscriptions do not survive a session that was
// created with CleanStart and SessionExpiryInterval 0.
type subscription struct {
	filter string
	cb     func(topic string, payload []byte)
}

// Publisher owns one broker connection for the life of the daemon.
//
// It is safe for concurrent use. The poller publishes readings from its own
// goroutine while the connection-up sequence runs from another, and Shutdown
// may arrive from the signal handler at any point in either.
type Publisher struct {
	cfg Config
	url *url.URL
	log *slog.Logger

	// offlineConfirmed is the whole design in one bit: it is true only once a
	// PUBACK for the retained "offline" is in hand, and it is the sole input to
	// the DISCONNECT reason code. Every teardown path consults it, including
	// ones we did not write.
	offlineConfirmed atomic.Bool

	// cm is written twice with the same pointer — once from the connection-up
	// callback (which autopaho may invoke before NewConnection has returned) and
	// once by Start. An atomic pointer makes that harmless.
	cm atomic.Pointer[autopaho.ConnectionManager]

	mu         sync.Mutex // guards the fields below
	started    bool
	cancelConn context.CancelFunc
	subs       []subscription
	upFns      []func()

	// upSlot serialises connection-up sequences and lets Shutdown wait for one
	// with a deadline, which a plain mutex cannot do. Capacity 1.
	upSlot chan struct{}

	// closing/closed stop a reconnection from republishing the birth "online"
	// after Shutdown has published "offline". Without them a reconnect landing
	// in that window would leave the retained status saying online for a process
	// that is already gone — the exact bug this package exists to prevent,
	// reintroduced through the back door.
	closing atomic.Bool
	closed  chan struct{}

	shutdownOnce sync.Once
	shutdownErr  error
}

// New validates cfg and returns a Publisher that has not yet connected.
func New(cfg Config) (*Publisher, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Publisher{
		cfg: cfg,
		// JoinHostPort rather than concatenation so an IPv6 literal survives.
		url:    &url.URL{Scheme: "mqtt", Host: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))},
		log:    logger,
		upSlot: make(chan struct{}, 1),
		closed: make(chan struct{}),
	}, nil
}

// Start registers the will and begins connecting. It does not block and does
// not report an unreachable broker: retries happen in the background until
// Shutdown.
//
// Non-blocking is a requirement, not a convenience. The Python opened the
// serial port first, retrying for up to 36 seconds, and only then set up MQTT —
// so during the window where the daemon was most likely to die (a sensor that
// never enumerates) no will was registered and the broker had nothing to
// publish on its behalf. Starting MQTT first means the LWT is armed before any
// hardware is touched.
//
// The connection manager's lifetime is deliberately detached from ctx via
// context.WithoutCancel. If it were a child, SIGTERM would cancel it
// concurrently with Shutdown, and autopaho would race us to send its own clean
// DISCONNECT before the "offline" publish completed. Shutdown is therefore the
// only way to stop the manager — which costs nothing in a daemon, because a
// process that exits without calling it drops the socket and the will fires.
func (p *Publisher) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mqttpub: start: %w", err)
	}
	if p.closing.Load() {
		// A Publisher is not reusable: the closing flag would reject every
		// publish on the new connection, so restarting has to mean a new
		// Publisher rather than a connection that silently does nothing.
		return ErrShutdown
	}

	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return ErrAlreadyStarted
	}
	p.started = true
	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.cancelConn = cancel
	p.mu.Unlock()

	cm, err := autopaho.NewConnection(connCtx, p.clientConfig())
	if err != nil {
		cancel()
		p.mu.Lock()
		p.started = false
		p.cancelConn = nil
		p.mu.Unlock()
		return fmt.Errorf("mqttpub: start: %w", err)
	}
	p.cm.Store(cm)
	return nil
}

// clientConfig builds the autopaho configuration. Kept separate from Start so
// the connection settings read as one block.
func (p *Publisher) clientConfig() autopaho.ClientConfig {
	return autopaho.ClientConfig{
		ServerUrls: []*url.URL{p.url},
		KeepAlive:  p.cfg.KeepAlive,

		// A fresh session every time the daemon starts, discarded the moment the
		// connection closes. There is nothing worth resuming: every publish is a
		// retained current value, so an inflight message that outlived a restart
		// would be delivering a stale reading.
		//
		// The zero SessionExpiryInterval is also what closes the other half of
		// the no-stale-readings rule. Declining PublishViaQueue is not enough on
		// its own, because a QoS 1 publish that was accepted into paho's session
		// state before the link dropped would be retransmitted on reconnection.
		// paho only keeps that state when the session outlives the connection
		// (paho/session/state/state.go:337 — connectionLost cleans the store
		// when the interval is zero), so with 0 the abandoned reading is dropped
		// rather than replayed as live. That is what
		// TestAbandonedPublishIsNotReplayedOnReconnect holds open across a drop.
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,

		// The non-deprecated spelling of ConnectRetryDelay.
		ReconnectBackoff: autopaho.NewConstantBackoff(p.cfg.ConnectRetryDelay),
		ConnectTimeout:   p.cfg.ConnectTimeout,

		ConnectUsername: p.cfg.Username,
		ConnectPassword: []byte(p.cfg.Password),

		// The will. QoS 1 and retained so it survives a subscriber that is not
		// connected at the time, which is the whole point — grow-app reads the
		// retained status on startup.
		//
		// WillProperties is left zero: a non-zero WillDelayInterval would hold
		// the will back, and with SessionExpiryInterval 0 the session ends the
		// instant the connection does, so the delay would only ever be latency
		// we chose to add.
		WillMessage: &paho.WillMessage{
			Topic:   p.cfg.StatusTopic,
			Payload: []byte(PayloadOffline),
			QoS:     1,
			Retain:  true,
		},
		WillProperties: &paho.WillProperties{},

		DisconnectPacketBuilder: p.disconnectPacket,
		OnConnectionUp:          p.onConnectionUp,
		OnConnectionDown:        p.onConnectionDown,
		OnConnectError: func(err error) {
			p.log.Warn("mqtt connection attempt failed", "broker", p.url.Host, "error", err)
		},
		Errors:     logAdapter{p.log, slog.LevelWarn},
		PahoErrors: logAdapter{p.log, slog.LevelDebug},

		ClientConfig: paho.ClientConfig{
			ClientID: ClientID(p.cfg.NodeID),
			// Registered here rather than through AddOnPublishReceived so an
			// inbound message on the very first connection is already routed.
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){p.onPublishReceived},
		},
	}
}

// disconnectPacket chooses the DISCONNECT reason code, and is the single point
// at which this package's rule is enforced.
//
// autopaho calls it on every teardown, including ones we did not initiate, so
// there is no path that reaches the broker without this decision being made.
func (p *Publisher) disconnectPacket() *paho.Disconnect {
	if p.offlineConfirmed.Load() {
		// The retained "offline" is durably on the broker; a will would only
		// duplicate it. 0x00 asks the broker to discard the will.
		return &paho.Disconnect{ReasonCode: disconnectNormal}
	}
	// We could not confirm "offline" — either we never published it, or we
	// published it and did not get a PUBACK. Have the broker publish the will
	// on our behalf. Note that returning nil here does NOT do this: autopaho
	// closes the connection cleanly anyway and the broker drops the will.
	return &paho.Disconnect{ReasonCode: disconnectWithWill}
}

// AwaitConnection blocks until the connection is up or ctx is done.
func (p *Publisher) AwaitConnection(ctx context.Context) error {
	cm := p.cm.Load()
	if cm == nil {
		return ErrNotStarted
	}
	if err := cm.AwaitConnection(ctx); err != nil {
		return fmt.Errorf("mqttpub: await connection: %w", err)
	}
	return nil
}

// PublishRetained publishes payload to topic at QoS 1 with the retain flag, and
// blocks until the PUBACK arrives or ctx is done.
//
// Every value this daemon emits is a current reading or a current availability,
// so retained is always right: a subscriber that connects late must see the last
// value rather than wait a poll interval for the next one.
//
// If the connection is down this returns [autopaho.ConnectionDownError]
// immediately rather than queueing. See the package doc.
func (p *Publisher) PublishRetained(ctx context.Context, topic string, payload []byte) error {
	if p.closing.Load() {
		return ErrShutdown
	}
	return p.publish(ctx, topic, payload)
}

// publish is PublishRetained without the shutdown guard, so that Shutdown's own
// "offline" is not rejected by the flag Shutdown just set.
func (p *Publisher) publish(ctx context.Context, topic string, payload []byte) error {
	cm := p.cm.Load()
	if cm == nil {
		return ErrNotStarted
	}
	if _, err := cm.Publish(ctx, &paho.Publish{
		Topic:   topic,
		Payload: payload,
		QoS:     1,
		Retain:  true,
	}); err != nil {
		return fmt.Errorf("mqttpub: publish %q: %w", topic, err)
	}
	return nil
}

// Subscribe registers cb for topic (an MQTT filter, wildcards allowed) and
// sends the SUBSCRIBE if the connection is up. The subscription is remembered
// and re-issued on every reconnection.
//
// Calling it before Start, or while disconnected, is not an error: the
// SUBSCRIBE is sent by the next connection-up sequence. Only a broker that
// refuses the filter is reported. A caller's own expired context is reported
// too — that deadline was the caller's choice, and a SUBSCRIBE written to a
// socket that then died is indistinguishable from one a wedged broker is
// sitting on.
//
// cb runs on the network goroutine that received the PUBLISH, so it must not
// block; hand the work to a channel if it might.
func (p *Publisher) Subscribe(ctx context.Context, topic string, cb func(topic string, payload []byte)) error {
	if topic == "" {
		return errors.New("mqttpub: subscribe: empty topic filter")
	}
	if cb == nil {
		return errors.New("mqttpub: subscribe: nil callback")
	}
	if p.closing.Load() {
		return ErrShutdown
	}

	p.mu.Lock()
	p.subs = append(p.subs, subscription{filter: topic, cb: cb})
	p.mu.Unlock()

	cm := p.cm.Load()
	if cm == nil {
		return nil // Start has not run yet; the connection-up sequence will send it.
	}
	err := p.sendSubscribe(ctx, cm, topic)
	if connectionUnavailable(err) {
		// The subscription is registered and the connection-up sequence will
		// issue it, so a down connection is a state rather than a failure.
		// Reporting it would make every caller special-case the ordinary case of
		// subscribing during startup, before the first connection completes.
		return nil
	}
	return err
}

// connectionUnavailable reports whether err means "there is no usable
// connection at this instant" as opposed to "the request was refused".
//
// It takes two sentinels because autopaho and paho each own one half of the
// reconnection. autopaho returns [autopaho.ConnectionDownError] only when its
// ConnectionManager holds no paho client at all (autopaho/auto.go:436) — before
// the first connection, and again once a dead one has finished tearing down.
// Between those, the client object outlives its connection: the manager passes
// the request straight through and paho's session answers
// [session.ErrNoConnection] (paho/session/state/state.go:360), which
// ConnectionDownError does not wrap.
//
// Both are the same fact, and treating only the first as "not an error" makes
// the second surface as a spurious failure during a routine reconnection. The
// window is narrow — autopaho nils its client immediately after the paho client
// finishes shutting down — but it is entered on every single reconnection, and
// this daemon is expected to run for months.
func connectionUnavailable(err error) bool {
	return errors.Is(err, autopaho.ConnectionDownError) || errors.Is(err, session.ErrNoConnection)
}

// sendSubscribe issues one SUBSCRIBE at QoS 1.
func (p *Publisher) sendSubscribe(ctx context.Context, cm *autopaho.ConnectionManager, filter string) error {
	if _, err := cm.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{{Topic: filter, QoS: 1}},
	}); err != nil {
		return fmt.Errorf("mqttpub: subscribe %q: %w", filter, err)
	}
	return nil
}

// OnConnectionUp registers fn to run after every successful connection,
// including reconnections. Callbacks run in registration order, one at a time,
// on a goroutine of their own — so unlike autopaho's own callback they may
// block and may publish.
//
// This is where discovery and the birth "online" belong. Both have to be
// re-sent on every reconnection: the session is created with CleanStart, so a
// broker that restarted has forgotten our retained messages entirely.
func (p *Publisher) OnConnectionUp(fn func()) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	p.upFns = append(p.upFns, fn)
	p.mu.Unlock()
}

// onConnectionUp is autopaho's callback. It must not block, so all it does is
// record the manager and hand off.
func (p *Publisher) onConnectionUp(cm *autopaho.ConnectionManager, _ *paho.Connack) {
	p.cm.Store(cm)
	p.log.Info("mqtt connected", "broker", p.url.Host, "client_id", ClientID(p.cfg.NodeID))
	go p.runConnectionUp(cm)
}

// onConnectionDown is autopaho's callback for a dropped connection. Returning
// true keeps it retrying, which is always what a daemon wants.
func (p *Publisher) onConnectionDown() bool {
	p.log.Warn("mqtt connection lost", "broker", p.url.Host)
	return true
}

// runConnectionUp re-establishes subscriptions and then runs the registered
// callbacks in order.
//
// Subscriptions go first: a callback that publishes a state whose command topic
// we have not yet subscribed to would miss a reply sent immediately after.
func (p *Publisher) runConnectionUp(cm *autopaho.ConnectionManager) {
	// One sequence at a time. If a reconnection lands while the previous
	// sequence is still running, it waits rather than interleaving — otherwise
	// two generations of birth messages could publish out of order.
	select {
	case p.upSlot <- struct{}{}:
		defer func() { <-p.upSlot }()
	case <-p.closed:
		return
	}
	// Re-checked after acquiring: select picks uniformly among ready cases, so
	// the branch above can be taken even once closed is closed.
	if p.closing.Load() {
		return
	}

	p.mu.Lock()
	subs := slices.Clone(p.subs)
	fns := slices.Clone(p.upFns)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ConnectTimeout)
	defer cancel()
	for _, s := range subs {
		if err := p.sendSubscribe(ctx, cm, s.filter); err != nil {
			p.log.Warn("mqtt resubscribe failed", "filter", s.filter, "error", err)
		}
	}

	for _, fn := range fns {
		if p.closing.Load() {
			return
		}
		fn()
	}
}

// onPublishReceived fans an inbound PUBLISH out to every matching subscription.
func (p *Publisher) onPublishReceived(pr paho.PublishReceived) (bool, error) {
	topic := pr.Packet.Topic

	p.mu.Lock()
	subs := slices.Clone(p.subs)
	p.mu.Unlock()

	handled := false
	for _, s := range subs {
		if topicMatches(s.filter, topic) {
			s.cb(topic, pr.Packet.Payload)
			handled = true
		}
	}
	return handled, nil
}

// Shutdown publishes the retained "offline" and then tears the connection down,
// in that order, and is the reason this package exists.
//
// The sequence is:
//
//  1. Stop new birth sequences, and let an in-flight one finish.
//  2. Publish retained "offline" to statusTopic and wait for the PUBACK.
//  3. Record whether that succeeded, which selects the DISCONNECT reason code.
//  4. Cancel the connection manager and wait, bounded, for it to finish.
//
// Every step is bounded by Config.ShutdownTimeout in total. A returned error
// means "offline" could not be confirmed, in which case the will has been asked
// to fire — the status still ends up correct, just via the broker.
//
// statusTopic must be Config.StatusTopic, or empty, which means the same thing.
// It is a check on the caller rather than a choice: the will was registered on
// Config.StatusTopic at CONNECT and cannot be re-pointed now, so publishing
// "offline" anywhere else would confirm a topic the will does not cover. The
// reason code would then be 0x00, the will would be discarded, and the real
// status topic would be left saying "online" forever — this package's own
// defect, reachable through a parameter. A mismatch is therefore refused with
// [ErrStatusTopicMismatch]: no publish is made, so the will fires and the
// status still ends up correct.
//
// ctx is used only for its values: the deadline is built with
// context.WithoutCancel because by the time Shutdown runs, ctx is the SIGTERM
// context and is already cancelled. A child of it would make every publish here
// fail instantly, which is the single easiest way to reintroduce the defect
// this package fixes.
func (p *Publisher) Shutdown(ctx context.Context, statusTopic string) error {
	p.shutdownOnce.Do(func() { p.shutdownErr = p.shutdown(ctx, statusTopic) })
	return p.shutdownErr
}

func (p *Publisher) shutdown(ctx context.Context, statusTopic string) error {
	// Checked before anything else, but acted on at step 2: a caller that got
	// the topic wrong still needs the connection torn down, and tearing it down
	// with the reason code left at 0x04 is what makes the will cover for us.
	var topicErr error
	if statusTopic != "" && statusTopic != p.cfg.StatusTopic {
		topicErr = fmt.Errorf("%w: Shutdown was given %q but the will is registered on %q",
			ErrStatusTopicMismatch, statusTopic, p.cfg.StatusTopic)
	}

	base := context.WithoutCancel(ctx)
	deadline := time.Now().Add(p.cfg.ShutdownTimeout)
	reserve := p.cfg.ShutdownTimeout / teardownShare

	// 1. No further birth messages, and let any in-flight one drain so it cannot
	//    republish "online" behind our back.
	p.closing.Store(true)
	close(p.closed)
	p.quiesce(base, reserve)

	// 2. The publish that the entire teardown is ordered around. Skipped
	//    entirely on a topic mismatch — the only safe response to "publish
	//    'offline' over here" is to publish it nowhere and let the will handle
	//    the topic it was actually registered on.
	err := topicErr
	if err == nil {
		pubCtx, cancelPub := context.WithDeadline(base, deadline.Add(-reserve))
		err = p.publish(pubCtx, p.cfg.StatusTopic, []byte(PayloadOffline))
		cancelPub()
	}

	// 3. The one bit that decides the reason code. Only a PUBACK counts.
	p.offlineConfirmed.Store(err == nil)
	if err != nil {
		p.log.Warn("mqtt retained offline not confirmed; will asked to fire",
			"topic", p.cfg.StatusTopic, "error", err)
	}

	// 4. Now, and only now, tear the connection down.
	p.mu.Lock()
	cancelConn := p.cancelConn
	p.mu.Unlock()
	if cancelConn == nil {
		return err // never started; nothing to disconnect
	}
	cancelConn()

	if cm := p.cm.Load(); cm != nil {
		waitCtx, cancelWait := context.WithDeadline(base, deadline)
		defer cancelWait()
		select {
		case <-cm.Done():
		case <-waitCtx.Done():
			// The DISCONNECT may not have been written. Dropping the socket
			// still produces the will, so the status is correct either way.
			err = errors.Join(err, fmt.Errorf("mqttpub: connection manager did not stop within %s", p.cfg.ShutdownTimeout))
		}
	}
	return err
}

// quiesce waits up to d for any in-flight connection-up sequence to finish. The
// slot is never released: after Shutdown there is nothing left to sequence.
func (p *Publisher) quiesce(base context.Context, d time.Duration) {
	ctx, cancel := context.WithTimeout(base, d)
	defer cancel()
	select {
	case p.upSlot <- struct{}{}:
	case <-ctx.Done():
		p.log.Warn("mqtt connection-up sequence still running at shutdown")
	}
}

// logAdapter bridges paho's Println/Printf logger onto slog at a fixed level.
type logAdapter struct {
	l     *slog.Logger
	level slog.Level
}

var _ log.Logger = logAdapter{}

func (a logAdapter) Println(v ...any) {
	a.l.Log(context.Background(), a.level, strings.TrimSuffix(fmt.Sprintln(v...), "\n"))
}

func (a logAdapter) Printf(format string, v ...any) {
	a.l.Log(context.Background(), a.level, strings.TrimSuffix(fmt.Sprintf(format, v...), "\n"))
}

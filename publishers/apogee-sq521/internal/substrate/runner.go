package substrate

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

// Measurer is the subset of an SDI-12 client this package needs, satisfied as-is
// by *sdi12.Client. Taking an interface rather than the concrete client is what
// lets the whole runner be exercised without a serial port.
//
// It names sdi12.Identity rather than a local echo of it because Go matches
// method sets nominally: a redeclared struct with identical fields would NOT
// make *sdi12.Client satisfy this, it would force an adapter at every call site.
type Measurer interface {
	Address() byte
	Measure(ctx context.Context, sub string) ([]float64, error)
	Identify(ctx context.Context) (sdi12.Identity, error)
}

// Publisher is the MQTT surface one probe needs. *mqttpub.Publisher satisfies it.
type Publisher interface {
	Start(ctx context.Context) error
	PublishRetained(ctx context.Context, topic string, payload []byte) error
	OnConnectionUp(fn func())
	Shutdown(ctx context.Context, statusTopic string) error
}

// NewPublisher builds the Publisher for one probe. It is injected so this
// package never imports mqttpub, and so a test can hand back a recorder.
type NewPublisher func(nodeID, statusTopic string) (Publisher, error)

// offlineAfter is how many consecutive failed polls mark a probe unavailable.
// Substrate moves slowly and the runner is called on a subdivision of the PAR
// cycle, so this is minutes rather than seconds — deliberately longer-suffering
// than the PAR sensor, whose reading is worthless the moment it is stale.
const offlineAfter = 3

// Runner owns every probe on the bus: their publishers, their retained state and
// their failure accounting.
//
// It is deliberately separate from the PAR daemon's App rather than merged into
// it. App keeps four maps keyed by object id alone, and its per-cycle success is
// one boolean across every reading — so a merged working set would let a healthy
// probe mask the PAR sensor going mute, leaving a frozen PPFD retained as
// "online" indefinitely. Nothing here touches that state; a probe failing is
// visible only on that probe's own topics.
type Runner struct {
	prefix string
	log    *slog.Logger
	probes []*probeState
}

type probeState struct {
	probe  Probe
	pub    Publisher
	topics entities.Topics
	dev    entities.Device
	table  []entities.Entity

	// born is whether discovery and the retained "online" have been published on
	// the current connection. Written only by the poll goroutine.
	born bool
	// rebirth is set from mqttpub's connection-up goroutine and consumed by the
	// poll goroutine. It exists so that goroutine touches nothing else: born,
	// lastPayload and announced are plain fields owned by the poller, and having
	// the callback write them directly was a data race that could also lose the
	// re-announce entirely. The PAR side solves the same problem by serialising
	// whole publish runs under App.seqMu; a hand-off flag is the cheaper answer
	// here, because this runner has exactly one consumer.
	rebirth atomic.Bool
	// lastPayload is what this process last wrote to each state topic, so an
	// unchanged value is not republished every cycle. The empty string is a
	// meaningful entry: it records that the topic currently holds a blank.
	lastPayload map[string]string
	// announced is the object ids whose discovery config is on the broker for
	// this connection. Optional entities are not in it until the probe has been
	// seen to report them; see announceOptional.
	announced map[string]bool
	// failures counts consecutive failed polls; at offlineAfter the probe's
	// retained availability flips.
	failures int
	// offline is the availability currently published, so the status topic is
	// written on transition rather than every cycle.
	offline bool
	serial  string
}

// NewRunner builds a Runner for the configured probes. It returns a nil Runner
// and no error when no probes are configured, so an unconfigured deployment
// carries no state and does no work.
func NewRunner(prefix string, probes []Probe, mk NewPublisher, log *slog.Logger) (*Runner, error) {
	if len(probes) == 0 {
		return nil, nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Runner{prefix: prefix, log: log}
	seenNode := map[string]bool{}
	seenAddr := map[byte]bool{}
	for _, p := range probes {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		// Two probes on one address cannot be told apart on the bus, and two on
		// one node id would fight over the same retained topics.
		if seenAddr[p.Address] {
			return nil, fmt.Errorf("substrate: two probes share address %q", p.Address)
		}
		if seenNode[p.NodeID] {
			return nil, fmt.Errorf("substrate: two probes share node id %q", p.NodeID)
		}
		seenAddr[p.Address], seenNode[p.NodeID] = true, true

		topics := p.Topics(prefix)
		pub, err := mk(p.NodeID, topics.Status())
		if err != nil {
			return nil, fmt.Errorf("substrate: publisher for %s: %w", p.NodeID, err)
		}
		ps := &probeState{
			probe:       p,
			pub:         pub,
			topics:      topics,
			dev:         p.Device("", ""),
			table:       Entities(),
			lastPayload: map[string]string{},
			announced:   map[string]bool{},
		}
		// A reconnect invalidates everything we believed was retained, so the
		// next poll re-announces rather than assuming the broker still holds it.
		// The flag is only raised here; the poll goroutine acts on it.
		pub.OnConnectionUp(func() { ps.rebirth.Store(true) })
		r.probes = append(r.probes, ps)
	}
	return r, nil
}

// Start brings up every probe's MQTT connection. Like the PAR daemon's, these
// dial in the background and retry forever, so this does not block on a broker.
func (r *Runner) Start(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for _, ps := range r.probes {
		if err := ps.pub.Start(ctx); err != nil {
			return fmt.Errorf("substrate: start %s: %w", ps.probe.NodeID, err)
		}
	}
	return nil
}

// Poll reads every probe once and publishes the result.
//
// at yields a conversation addressed to one probe on the already-open port. The
// caller owns the port and calls this from the same goroutine that polls the PAR
// sensor, which is what keeps SDI-12 transactions strictly sequential: the
// protocol has an unsolicited-transmit window between the measurement header and
// the data command, so two masters interleaving would collide on the wire even
// with a perfectly correct lock.
//
// It never returns an error. One probe failing is that probe's business, and
// reporting it upward would invite the caller to fold it into a shared failure
// ladder — the exact coupling this design exists to avoid.
func (r *Runner) Poll(ctx context.Context, at func(addr byte) Measurer) {
	if r == nil {
		return
	}
	for _, ps := range r.probes {
		if ctx.Err() != nil {
			return
		}
		r.pollOne(ctx, ps, at(ps.probe.Address))
	}
}

func (r *Runner) pollOne(ctx context.Context, ps *probeState, m Measurer) {
	log := r.log.With("node_id", ps.probe.NodeID, "address", string(ps.probe.Address))

	// mqttpub connects with CleanStart, so a broker we have just reconnected to
	// holds none of our retained topics — not the discovery configs, not the
	// serial, not a value that happens not to have changed since. Forgetting what
	// we believe is on the broker is what makes the rebirth republish all of it;
	// clearing only `born` would re-send discovery and "online" while
	// publishState went on suppressing every unchanged value as already present.
	if ps.rebirth.Swap(false) {
		ps.born = false
		clear(ps.lastPayload)
		clear(ps.announced)
	}

	if !ps.born {
		if err := r.birth(ctx, ps, m); err != nil {
			log.Warn("could not announce the probe; retrying next cycle", "error", err)
			r.recordFailure(ctx, ps, log)
			return
		}
	}

	readings, err := Read(ctx, m)
	if err != nil {
		log.Warn("substrate read failed", "error", err)
		r.recordFailure(ctx, ps, log)
		return
	}

	r.announceOptional(ctx, ps, readings, log)

	for _, rd := range readings {
		e, ok := entityFor(ps.table, rd.ObjectID)
		if !ok {
			continue
		}
		if !rd.OK {
			// A blank retracts the stale retained value. Leaving the last good
			// number would let grow-app keep rendering a pre-fault reading as
			// current, which is worse than showing nothing.
			log.Warn("discarding an invalid reading", "entity", rd.ObjectID, "reason", rd.Reason)
			r.publishState(ctx, ps, e, "", log)
			continue
		}
		r.publishState(ctx, ps, e, entities.Format(e, rd.Value), log)
	}

	r.recordSuccess(ctx, ps, log)
}

// birth publishes discovery for every entity, then the retained "online".
// Discovery precedes availability so a subscriber that sees the probe come up
// has already seen the entities it is about to receive values for.
func (r *Runner) birth(ctx context.Context, ps *probeState, m Measurer) error {
	if ps.serial == "" {
		// Identification is best-effort: a probe that measures but will not
		// identify is still worth publishing, so a failure here is recorded and
		// retried rather than fatal.
		if id, err := m.Identify(ctx); err == nil {
			ps.serial = id.Serial
			if id.Model != "" {
				ps.dev = ps.probe.Device(id.Model, "")
			}
			r.log.Info("substrate probe identified",
				"node_id", ps.probe.NodeID, "vendor", id.Vendor, "model", id.Model, "serial", id.Serial)
		}
	}

	for _, e := range ps.table {
		// An optional entity's capability is not known until the probe answers: a
		// TEROS 11 has no EC electrode and returns two values, so announcing bulk
		// EC here would leave a retained config whose state topic never fills —
		// the defect the PAR daemon's probe-before-discovery rule exists to
		// prevent. announceOptional publishes it once the probe reports it.
		if e.Optional {
			continue
		}
		if err := r.announce(ctx, ps, e); err != nil {
			return err
		}
	}

	if ps.serial != "" {
		if e, ok := entityFor(ps.table, ObjectSerial); ok {
			r.publishState(ctx, ps, e, ps.serial, r.log)
		}
	}

	if err := ps.pub.PublishRetained(ctx, ps.topics.Status(), []byte("online")); err != nil {
		return fmt.Errorf("publish availability: %w", err)
	}
	ps.offline = false
	ps.born = true
	return nil
}

// announce publishes one entity's retained discovery config and records it.
func (r *Runner) announce(ctx context.Context, ps *probeState, e entities.Entity) error {
	payload, err := entities.DiscoveryPayload(e, ps.topics, ps.dev)
	if err != nil {
		return fmt.Errorf("discovery payload for %s: %w", e.ObjectID, err)
	}
	if err := ps.pub.PublishRetained(ctx, ps.topics.Discovery(e.Component, e.ObjectID), payload); err != nil {
		return fmt.Errorf("publish discovery for %s: %w", e.ObjectID, err)
	}
	ps.announced[e.ObjectID] = true
	return nil
}

// announceOptional publishes discovery for optional entities this probe has now
// been seen to report, which is the only evidence that it has the hardware.
//
// Called before the readings are published so a config always precedes the state
// topic it resolves. A probe that never reports the value never gets the config,
// which is the difference between "this sensor has no EC electrode" and "this
// sensor's EC is unknown".
func (r *Runner) announceOptional(ctx context.Context, ps *probeState, readings []Reading, log *slog.Logger) {
	for _, rd := range readings {
		e, ok := entityFor(ps.table, rd.ObjectID)
		if !ok || !e.Optional || ps.announced[e.ObjectID] {
			continue
		}
		if err := r.announce(ctx, ps, e); err != nil {
			log.Warn("could not announce an optional entity; retrying next cycle",
				"entity", e.ObjectID, "error", err)
			continue
		}
		log.Info("probe reports an optional value; announced it",
			"entity", e.ObjectID)
	}
}

// publishState writes one entity's payload, skipping the write when the topic
// already holds it. Retained topics are idempotent, but republishing an
// unchanged value wakes every subscriber and rewrites the broker's on-disk
// retained set on every cycle.
func (r *Runner) publishState(ctx context.Context, ps *probeState, e entities.Entity, payload string, log *slog.Logger) {
	if prev, ok := ps.lastPayload[e.ObjectID]; ok && prev == payload {
		return
	}
	topic := ps.topics.State(e.Component, e.ObjectID)
	if err := ps.pub.PublishRetained(ctx, topic, []byte(payload)); err != nil {
		log.Warn("publish failed", "topic", topic, "error", err)
		return
	}
	ps.lastPayload[e.ObjectID] = payload
}

func (r *Runner) recordSuccess(ctx context.Context, ps *probeState, log *slog.Logger) {
	if ps.offline {
		if err := ps.pub.PublishRetained(ctx, ps.topics.Status(), []byte("online")); err != nil {
			log.Warn("could not publish recovery", "error", err)
			return
		}
		log.Info("substrate probe is available again", "after_failures", ps.failures)
		ps.offline = false
	}
	ps.failures = 0
}

func (r *Runner) recordFailure(ctx context.Context, ps *probeState, log *slog.Logger) {
	ps.failures++
	if ps.offline || ps.failures < offlineAfter {
		return
	}
	if err := ps.pub.PublishRetained(ctx, ps.topics.Status(), []byte("offline")); err != nil {
		log.Warn("could not publish unavailability", "error", err)
		return
	}
	// Blanking the polled values as well as the status is what makes grow-app
	// drop the probe rather than render its last reading as live.
	for _, e := range ps.table {
		if e.Kind == entities.SourcePolled {
			r.publishState(ctx, ps, e, "", log)
		}
	}
	log.Warn("substrate probe is unavailable; its readings are invalidated",
		"failed_polls", ps.failures, "topic", ps.topics.Status())
	ps.offline = true
}

// Shutdown drains every probe's retained "offline" and disconnects.
func (r *Runner) Shutdown(ctx context.Context) {
	if r == nil {
		return
	}
	for _, ps := range r.probes {
		if err := ps.pub.Shutdown(ctx, ps.topics.Status()); err != nil {
			r.log.Warn("substrate publisher shutdown failed", "node_id", ps.probe.NodeID, "error", err)
		}
	}
}

// NodeIDs lists the configured probes, for the startup log and --check-config.
func (r *Runner) NodeIDs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.probes))
	for _, ps := range r.probes {
		out = append(out, ps.probe.NodeID)
	}
	return out
}

func entityFor(table []entities.Entity, objectID string) (entities.Entity, bool) {
	for _, e := range table {
		if e.ObjectID == objectID {
			return e, true
		}
	}
	return entities.Entity{}, false
}

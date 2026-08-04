package substrate

import (
	"context"
	"fmt"
	"log/slog"

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

	// born is whether discovery and the retained "online" have been published
	// for this connection. Cleared on every connection-up, because a broker
	// that lost us also lost whatever we believed was retained.
	born bool
	// lastPayload is what this process last wrote to each state topic, so an
	// unchanged value is not republished every cycle. The empty string is a
	// meaningful entry: it records that the topic currently holds a blank.
	lastPayload map[string]string
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
		}
		// A reconnect invalidates everything we believed was retained, so the
		// next poll re-announces rather than assuming the broker still holds it.
		pub.OnConnectionUp(func() { ps.born = false })
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
		payload, err := entities.DiscoveryPayload(e, ps.topics, ps.dev)
		if err != nil {
			return fmt.Errorf("discovery payload for %s: %w", e.ObjectID, err)
		}
		if err := ps.pub.PublishRetained(ctx, ps.topics.Discovery(e.Component, e.ObjectID), payload); err != nil {
			return fmt.Errorf("publish discovery for %s: %w", e.ObjectID, err)
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

package app

import (
	"context"
	"testing"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/substrate"
)

// busSession is a Session that shares its port, as the production serialSession
// does. Everything it hands out is the same recording measurer, so a test can
// count how often the probes were actually asked.
type busSession struct {
	Session
	guest *countingMeasurer
}

func (b *busSession) At(byte) substrate.Measurer { return b.guest }

type countingMeasurer struct {
	reads int
}

func (m *countingMeasurer) Address() byte { return 'A' }

func (m *countingMeasurer) Measure(context.Context, string) ([]float64, error) {
	m.reads++
	return []float64{2852.7, 25.8, 24}, nil
}

func (m *countingMeasurer) Identify(context.Context) (sdi12.Identity, error) {
	return sdi12.Identity{Vendor: "METER", Model: "TER12", Serial: "T12-00065327"}, nil
}

// substratePub is the minimum substrate.Publisher: it accepts everything and
// remembers nothing, because these tests are about when the bus is polled
// rather than what lands on it.
type substratePub struct{}

func (substratePub) Start(context.Context) error                           { return nil }
func (substratePub) PublishRetained(context.Context, string, []byte) error { return nil }
func (substratePub) OnConnectionUp(func())                                 {}
func (substratePub) Shutdown(context.Context, string) error                { return nil }

func newTestRunner(t *testing.T) *substrate.Runner {
	t.Helper()
	r, err := substrate.NewRunner("grow/daniel-home",
		[]substrate.Probe{{Address: 'A', NodeID: "substrate-a"}},
		func(string, string) (substrate.Publisher, error) { return substratePub{}, nil }, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// The default deployment. SUBSTRATE_PROBES unset means a nil Runner, and every
// entry point must be inert — this is what makes the feature provably free for
// anyone not using it.
func TestPollSubstrateIsInertWhenUnconfigured(t *testing.T) {
	r := newRig(t)
	if r.App.deps.Substrate != nil {
		t.Fatal("rig should build with no substrate configured")
	}
	guest := &countingMeasurer{}
	s := &busSession{Session: newHealthySession(), guest: guest}
	st := &pollState{}

	for range 50 {
		r.App.pollSubstrate(context.Background(), s, st)
	}
	if guest.reads != 0 {
		t.Errorf("polled the bus %d times with no Runner configured, want 0", guest.reads)
	}
}

// A Session that does not share its port cannot host a guest. Only the
// production serialSession can, so this is the path every existing test takes.
func TestPollSubstrateSkipsASessionThatIsNotABus(t *testing.T) {
	r := newRig(t)
	r.App.deps.Substrate = newTestRunner(t)
	r.App.cfg.SubstrateEvery = 1

	st := &pollState{}
	for range 10 {
		// newHealthySession is a plain Session with no At method.
		r.App.pollSubstrate(context.Background(), newHealthySession(), st)
	}
	// Nothing to assert beyond "did not panic and did not type-assert wrongly";
	// reaching here is the assertion.
}

// The subdivision. Substrate moves over hours and each probe costs an SDI-12
// transaction inside the PAR sensor's cycle budget, so the probes are polled
// every Nth cycle rather than every one.
func TestPollSubstrateHonoursTheSubdivision(t *testing.T) {
	const every = 15

	r := newRig(t)
	r.App.deps.Substrate = newTestRunner(t)
	r.App.cfg.SubstrateEvery = every

	guest := &countingMeasurer{}
	s := &busSession{Session: newHealthySession(), guest: guest}
	st := &pollState{}

	for i := 1; i < every; i++ {
		r.App.pollSubstrate(context.Background(), s, st)
		if guest.reads != 0 {
			t.Fatalf("polled on cycle %d, want the first read on cycle %d", i, every)
		}
	}
	r.App.pollSubstrate(context.Background(), s, st)
	if guest.reads != 1 {
		t.Fatalf("reads after %d cycles = %d, want 1", every, guest.reads)
	}

	// And again on the next boundary, not before it.
	for i := 1; i < every; i++ {
		r.App.pollSubstrate(context.Background(), s, st)
	}
	if guest.reads != 1 {
		t.Errorf("reads = %d before the second boundary, want 1", guest.reads)
	}
	r.App.pollSubstrate(context.Background(), s, st)
	if guest.reads != 2 {
		t.Errorf("reads = %d after the second boundary, want 2", guest.reads)
	}
}

// A zero subdivision can only come from a hand-built Config — Load validates it
// — and must degrade to "every cycle" rather than dividing by zero or never
// polling at all.
func TestPollSubstrateTreatsAZeroSubdivisionAsEveryCycle(t *testing.T) {
	r := newRig(t)
	r.App.deps.Substrate = newTestRunner(t)
	r.App.cfg.SubstrateEvery = 0

	guest := &countingMeasurer{}
	s := &busSession{Session: newHealthySession(), guest: guest}
	st := &pollState{}

	for range 3 {
		r.App.pollSubstrate(context.Background(), s, st)
	}
	if guest.reads != 3 {
		t.Errorf("reads = %d, want 3 (one per cycle)", guest.reads)
	}
}

// A cancelled context must stop the probes as promptly as it stops the PAR
// sensor, rather than starting a fresh transaction on the way out.
func TestPollSubstrateStopsOnCancellation(t *testing.T) {
	r := newRig(t)
	r.App.deps.Substrate = newTestRunner(t)
	r.App.cfg.SubstrateEvery = 1

	guest := &countingMeasurer{}
	s := &busSession{Session: newHealthySession(), guest: guest}
	st := &pollState{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.App.pollSubstrate(ctx, s, st)

	if guest.reads != 0 {
		t.Errorf("polled %d times after cancellation, want 0", guest.reads)
	}
}

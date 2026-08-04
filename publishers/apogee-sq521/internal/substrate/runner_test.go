package substrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

type fakePub struct {
	last map[string]string
	// seq is every topic published, in order, so a test can assert that a
	// discovery config precedes the state topic it resolves.
	seq     []string
	writes  int
	onUp    func()
	started bool
	downTo  string
}

func newFakePub() *fakePub { return &fakePub{last: map[string]string{}} }

func (p *fakePub) Start(context.Context) error { p.started = true; return nil }

func (p *fakePub) PublishRetained(_ context.Context, topic string, payload []byte) error {
	p.last[topic] = string(payload)
	p.seq = append(p.seq, topic)
	p.writes++
	return nil
}

func (p *fakePub) OnConnectionUp(fn func()) { p.onUp = fn }

func (p *fakePub) Shutdown(_ context.Context, statusTopic string) error {
	p.downTo = statusTopic
	return nil
}

type fakeMeasurer struct {
	addr     byte
	values   []float64
	err      error
	ident    sdi12.Identity
	identErr error
	reads    int
}

func (m *fakeMeasurer) Address() byte { return m.addr }

func (m *fakeMeasurer) Measure(context.Context, string) ([]float64, error) {
	m.reads++
	if m.err != nil {
		return nil, m.err
	}
	return m.values, nil
}

func (m *fakeMeasurer) Identify(context.Context) (sdi12.Identity, error) {
	if m.identErr != nil {
		return sdi12.Identity{}, m.identErr
	}
	return m.ident, nil
}

// harness wires one or more probes to fake publishers and measurers.
type harness struct {
	t    *testing.T
	run  *Runner
	pubs map[string]*fakePub
	meas map[byte]*fakeMeasurer
}

func newHarness(t *testing.T, probes ...Probe) *harness {
	t.Helper()
	h := &harness{t: t, pubs: map[string]*fakePub{}, meas: map[byte]*fakeMeasurer{}}
	r, err := NewRunner("grow/daniel-home", probes, func(nodeID, _ string) (Publisher, error) {
		p := newFakePub()
		h.pubs[nodeID] = p
		return p, nil
	}, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.run = r
	for _, p := range probes {
		h.meas[p.Address] = &fakeMeasurer{
			addr:   p.Address,
			values: []float64{2852.7, 25.8, 24},
			ident:  sdi12.Identity{Vendor: "METER", Model: "TER12", Serial: "T12-000653" + string(p.Address)},
		}
	}
	return h
}

func (h *harness) poll() {
	h.run.Poll(context.Background(), func(addr byte) Measurer { return h.meas[addr] })
}

func (h *harness) topic(node, suffix string) string {
	return "grow/daniel-home/" + node + suffix
}

func TestRunnerPublishesDiscoveryThenAvailabilityThenValues(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll()

	pub := h.pubs["substrate-a"]
	status := h.topic("substrate-a", "/status")
	if got := pub.last[status]; got != "online" {
		t.Errorf("status = %q, want online", got)
	}
	// Discovery must exist for every entity before values are believable.
	for _, id := range []string{ObjectRawCounts, ObjectTemperature, ObjectBulkEC, ObjectSerial} {
		found := false
		for topic := range pub.last {
			if strings.Contains(topic, id) && strings.HasSuffix(topic, "/config") {
				found = true
			}
		}
		if !found {
			t.Errorf("no discovery config published for %s", id)
		}
	}
	if got := pub.last[h.topic("substrate-a", "/sensor/"+ObjectRawCounts+"/state")]; got != "2852.70" {
		t.Errorf("raw counts state = %q, want 2852.70", got)
	}
	if got := pub.last[h.topic("substrate-a", "/sensor/"+ObjectBulkEC+"/state")]; got != "0.024" {
		t.Errorf("bulk EC state = %q, want 0.024 (24 µS/cm as mS/cm)", got)
	}
}

func TestRunnerBlanksAnInvalidReadingButKeepsTheRest(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	// Low voltage on bulk EC only.
	h.meas['A'].values = []float64{2852.7, 25.8, ErrCodeLowVoltage}
	h.poll()

	pub := h.pubs["substrate-a"]
	if got := pub.last[h.topic("substrate-a", "/sensor/"+ObjectBulkEC+"/state")]; got != "" {
		t.Errorf("bulk EC state = %q, want a blank retracting the stale value", got)
	}
	if got := pub.last[h.topic("substrate-a", "/sensor/"+ObjectTemperature+"/state")]; got != "25.8" {
		t.Errorf("temperature = %q, want it published — only bulk EC was invalid", got)
	}
	// The probe answered, so it is still available.
	if got := pub.last[h.topic("substrate-a", "/status")]; got != "online" {
		t.Errorf("status = %q, want online: the sensor responded", got)
	}
}

func TestRunnerGoesOfflineAfterRepeatedFailuresAndBlanksReadings(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll() // one good poll first, so there is a value to invalidate

	pub := h.pubs["substrate-a"]
	status := h.topic("substrate-a", "/status")
	h.meas['A'].err = errors.New("no response within budget")

	for i := 1; i < offlineAfter; i++ {
		h.poll()
		if got := pub.last[status]; got != "online" {
			t.Fatalf("after %d failures status = %q, want online until %d", i, got, offlineAfter)
		}
	}
	h.poll()
	if got := pub.last[status]; got != "offline" {
		t.Errorf("status = %q, want offline after %d failures", got, offlineAfter)
	}
	if got := pub.last[h.topic("substrate-a", "/sensor/"+ObjectRawCounts+"/state")]; got != "" {
		t.Errorf("raw counts = %q, want blank so grow-app drops it rather than rendering it live", got)
	}

	// Recovery flips availability back without needing a restart.
	h.meas['A'].err = nil
	h.poll()
	if got := pub.last[status]; got != "online" {
		t.Errorf("status = %q, want online again after a successful read", got)
	}
}

// The architectural guarantee: probes are independent. This is why each gets its
// own device and publisher rather than sharing the quantum sensor's.
func TestOneProbeFailingDoesNotAffectAnother(t *testing.T) {
	h := newHarness(t,
		Probe{Address: 'A', NodeID: "substrate-a"},
		Probe{Address: 'B', NodeID: "substrate-b"},
	)
	h.poll()
	h.meas['A'].err = errors.New("probe A unplugged")
	for i := 0; i <= offlineAfter; i++ {
		h.poll()
	}

	if got := h.pubs["substrate-a"].last[h.topic("substrate-a", "/status")]; got != "offline" {
		t.Errorf("probe A status = %q, want offline", got)
	}
	if got := h.pubs["substrate-b"].last[h.topic("substrate-b", "/status")]; got != "online" {
		t.Errorf("probe B status = %q, want online — A's failure is not B's", got)
	}
	if got := h.pubs["substrate-b"].last[h.topic("substrate-b", "/sensor/"+ObjectTemperature+"/state")]; got != "25.8" {
		t.Errorf("probe B temperature = %q, want it still publishing", got)
	}
}

func TestUnchangedValuesAreNotRepublished(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll()
	before := h.pubs["substrate-a"].writes
	h.poll()
	h.poll()
	if after := h.pubs["substrate-a"].writes; after != before {
		t.Errorf("writes went %d -> %d on unchanged values; retained republishing wakes every subscriber",
			before, after)
	}
}

func TestReconnectReannounces(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll()
	pub := h.pubs["substrate-a"]
	before := pub.writes

	// A broker that lost us also lost what we believed it retained.
	pub.onUp()
	h.poll()
	if pub.writes <= before {
		t.Error("no republish after connection-up; discovery would be missing from the broker")
	}
}

func TestNoProbesMeansNoRunner(t *testing.T) {
	r, err := NewRunner("grow/daniel-home", nil, func(string, string) (Publisher, error) {
		t.Fatal("must not build a publisher when no probes are configured")
		return nil, nil
	}, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r != nil {
		t.Fatal("want a nil Runner when unconfigured")
	}
	// Every entry point must be safe on the nil Runner, because that is how the
	// PAR daemon runs when SUBSTRATE_PROBES is unset.
	if err := r.Start(context.Background()); err != nil {
		t.Errorf("nil Start: %v", err)
	}
	r.Poll(context.Background(), func(byte) Measurer {
		t.Fatal("nil Runner must not poll")
		return nil
	})
	r.Shutdown(context.Background())
	if got := r.NodeIDs(); got != nil {
		t.Errorf("nil NodeIDs = %v", got)
	}
}

func TestDuplicateProbesRejected(t *testing.T) {
	mk := func(string, string) (Publisher, error) { return newFakePub(), nil }
	_, err := NewRunner("p", []Probe{
		{Address: 'A', NodeID: "substrate-a"},
		{Address: 'A', NodeID: "substrate-b"},
	}, mk, nil)
	if err == nil || !strings.Contains(err.Error(), "address") {
		t.Errorf("duplicate address: got %v, want an error naming the address", err)
	}

	_, err = NewRunner("p", []Probe{
		{Address: 'A', NodeID: "substrate-a"},
		{Address: 'B', NodeID: "substrate-a"},
	}, mk, nil)
	if err == nil || !strings.Contains(err.Error(), "node id") {
		t.Errorf("duplicate node id: got %v, want an error naming the node id", err)
	}
}

func TestShutdownDrainsEveryProbe(t *testing.T) {
	h := newHarness(t,
		Probe{Address: 'A', NodeID: "substrate-a"},
		Probe{Address: 'B', NodeID: "substrate-b"},
	)
	h.run.Shutdown(context.Background())
	for node, pub := range h.pubs {
		want := h.topic(node, "/status")
		if pub.downTo != want {
			t.Errorf("%s shutdown status topic = %q, want %q", node, pub.downTo, want)
		}
	}
}

func TestIdentifyFailureIsNotFatal(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.meas['A'].identErr = errors.New("no reply to AI!")
	h.poll()

	// A probe that measures but will not identify is still worth publishing.
	if got := h.pubs["substrate-a"].last[h.topic("substrate-a", "/sensor/"+ObjectTemperature+"/state")]; got != "25.8" {
		t.Errorf("temperature = %q, want the reading published despite identification failing", got)
	}
}

// ---------------------------------------------------------------------------
// Reconnect. mqttpub connects with CleanStart, so a broker we reconnect to holds
// none of our retained topics.
// ---------------------------------------------------------------------------

// The bug this covers: clearing only `born` re-sent discovery and "online" but
// left lastPayload populated, so publishState went on suppressing every value
// that happened not to have changed. The serial is the worst case — it is
// written once at birth and never changes, so it stayed blank in grow-app for
// the life of the connection.
func TestReconnectRepublishesUnchangedValues(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll()

	pub := h.pubs["substrate-a"]
	serialTopic := h.topic("substrate-a", "/sensor/"+ObjectSerial+"/state")
	countsTopic := h.topic("substrate-a", "/sensor/"+ObjectRawCounts+"/state")
	if pub.last[serialTopic] == "" {
		t.Fatal("serial was never published on the first birth")
	}

	// The broker forgets everything, then we reconnect and poll again with the
	// probe reporting exactly what it reported before.
	clear(pub.last)
	pub.onUp()
	h.poll()

	if got := pub.last[serialTopic]; got == "" {
		t.Error("serial was not republished after a reconnect; it is static, so it never changes and would stay blank forever")
	}
	if got := pub.last[countsTopic]; got == "" {
		t.Error("an unchanged reading was not republished after a reconnect")
	}
	if got := pub.last[h.topic("substrate-a", "/status")]; got != "online" {
		t.Errorf("status after reconnect = %q, want online", got)
	}
	if got := pub.last[h.topic("substrate-a", "/_discovery/sensor/substrate-a/"+ObjectRawCounts+"/config")]; got == "" {
		// Topic shape differs by prefix; assert discovery went out at all.
		found := false
		for topic := range pub.last {
			if strings.Contains(topic, "config") {
				found = true
				break
			}
		}
		if !found {
			t.Error("no discovery config was republished after a reconnect")
		}
	}
}

// The connection-up callback runs on mqttpub's goroutine while the poll
// goroutine owns every other field. Under -race this fails if the callback
// writes born/lastPayload directly, as it used to.
func TestReconnectCallbackIsRaceFreeAgainstPolling(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	pub := h.pubs["substrate-a"]

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			pub.onUp()
		}
	}()
	for range 200 {
		h.poll()
	}
	<-done
}

// ---------------------------------------------------------------------------
// Optional entities. A TEROS 11 has no EC electrode and returns two values.
// ---------------------------------------------------------------------------

func discoveryTopics(pub *fakePub) []string {
	var out []string
	for topic := range pub.last {
		if strings.Contains(topic, "/config") {
			out = append(out, topic)
		}
	}
	return out
}

// Announcing bulk EC for a probe that has no EC electrode leaves a retained
// config whose state topic never fills — a permanently-unknown entity in
// grow-app. The Optional flag existed for this and was never consulted.
func TestTeros11NeverGetsABulkEcConfig(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.meas['A'].values = []float64{2852.7, 25.8} // two values: a TEROS 11
	h.poll()
	h.poll()

	pub := h.pubs["substrate-a"]
	for _, topic := range discoveryTopics(pub) {
		if strings.Contains(topic, ObjectBulkEC) {
			t.Errorf("published a bulk-EC discovery config for a 2-value probe: %s", topic)
		}
	}
	if len(discoveryTopics(pub)) == 0 {
		t.Fatal("no discovery configs at all — the fixture is wrong, not the code")
	}
}

func TestTeros12GetsABulkEcConfigOnceItReportsOne(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll() // three values by default

	pub := h.pubs["substrate-a"]
	found := false
	for _, topic := range discoveryTopics(pub) {
		if strings.Contains(topic, ObjectBulkEC) {
			found = true
		}
	}
	if !found {
		t.Error("a probe that reported bulk EC never got its discovery config")
	}
}

// Discovery must precede the state topic it resolves, including for the optional
// row that is announced a beat later than the rest.
func TestOptionalConfigPrecedesItsState(t *testing.T) {
	h := newHarness(t, Probe{Address: 'A', NodeID: "substrate-a"})
	h.poll()

	pub := h.pubs["substrate-a"]
	config, state := -1, -1
	for i, topic := range pub.seq {
		if !strings.Contains(topic, ObjectBulkEC) {
			continue
		}
		if strings.Contains(topic, "/config") && config == -1 {
			config = i
		}
		if strings.Contains(topic, "/state") && state == -1 {
			state = i
		}
	}
	if config == -1 || state == -1 {
		t.Fatalf("expected both a config and a state publish for bulk EC, got config=%d state=%d", config, state)
	}
	if config > state {
		t.Error("bulk EC's state was published before its discovery config; a subscriber would see a value for an entity it does not know")
	}
}

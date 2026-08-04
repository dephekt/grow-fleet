package substrate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

func TestProbeValidate(t *testing.T) {
	tests := []struct {
		name    string
		probe   Probe
		wantErr string
	}{
		{"letter address", Probe{Address: 'A', NodeID: "substrate-a"}, ""},
		{"digit address", Probe{Address: '3', NodeID: "substrate-c"}, ""},
		{"empty node id", Probe{Address: 'A'}, "no node id"},
		{"bad address", Probe{Address: '!', NodeID: "substrate-a"}, "one character"},
		// grow-app's normalizeDiscoveryId collapses '-' to '_', so accepting an
		// underscored node id would let two spellings of one probe produce
		// colliding entity ids.
		{"underscored node id", Probe{Address: 'A', NodeID: "substrate_a"}, "must use '-' not '_'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.probe.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Every string in the table is a wire contract. grow-history-recorder keys
// InfluxDB series on (node id, object id), so a rename orphans history instead of
// migrating it — these are the constraints that make a rename necessary, checked
// here so they fail at build time rather than after a season of data.
func TestEntityTableContracts(t *testing.T) {
	table := Entities()
	if len(table) == 0 {
		t.Fatal("empty entity table")
	}

	seenID := map[string]bool{}
	seenIndex := map[int]string{}
	for _, e := range table {
		if seenID[e.ObjectID] {
			t.Errorf("duplicate object id %q", e.ObjectID)
		}
		seenID[e.ObjectID] = true

		// The prefix is what keeps a soil probe out of grow-app's CLIMATE card:
		// isAmbientTemperature rejects substrate|soil|medium|root, and a probe
		// reporting °C from inside a pot would otherwise satisfy it.
		if !strings.HasPrefix(e.ObjectID, "substrate_") {
			t.Errorf("object id %q must start with substrate_", e.ObjectID)
		}

		// isDangerousEntity substring-matches these and grow-app filters
		// dangerous entities out of its fallback metrics, so an entity named
		// this way would be invisible rather than merely oddly labelled.
		for _, banned := range []string{"calibration", "calibrate", "reset"} {
			if strings.Contains(e.ObjectID, banned) || strings.Contains(strings.ToLower(e.Name), banned) {
				t.Errorf("entity %q must not contain %q — grow-app would hide it", e.ObjectID, banned)
			}
		}

		if e.Component == "" {
			t.Errorf("entity %q has no component", e.ObjectID)
		}
		if e.Name == "" {
			t.Errorf("entity %q has no name", e.ObjectID)
		}

		// Two polled entities reading the same index would silently publish one
		// value under two names.
		if e.Kind == entities.SourcePolled {
			if prev, dup := seenIndex[e.Index]; dup {
				t.Errorf("entities %q and %q both read value index %d", prev, e.ObjectID, e.Index)
			}
			seenIndex[e.Index] = e.ObjectID
		}
	}

	for _, want := range []string{ObjectRawCounts, ObjectTemperature, ObjectBulkEC, ObjectSerial} {
		if !seenID[want] {
			t.Errorf("table is missing %q", want)
		}
	}
}

func TestRawCountsCarriesNoUnit(t *testing.T) {
	// Calibrated counts are dimensionless. Declaring a unit would invite a
	// consumer to treat them as a physical quantity, when they only become water
	// content after grow-app applies that zone's substrate equation.
	for _, e := range Entities() {
		if e.ObjectID == ObjectRawCounts && e.Unit != "" {
			t.Errorf("raw counts must be unitless, got unit %q", e.Unit)
		}
	}
}

func TestBulkECIsPublishedInMilliSiemens(t *testing.T) {
	for _, e := range Entities() {
		if e.ObjectID == ObjectBulkEC && e.Unit != "mS/cm" {
			t.Errorf("bulk EC unit = %q, want mS/cm (every grower-facing EC in this system is mS/cm)", e.Unit)
		}
	}
}

// The whole point of one MQTT device per probe: separate identifiers, so one
// probe's availability cannot speak for another's or for the PAR sensor's.
func TestEachProbeIsItsOwnDevice(t *testing.T) {
	a := Probe{Address: 'A', NodeID: "substrate-a", Name: "Substrate A"}
	b := Probe{Address: 'B', NodeID: "substrate-b", Name: "Substrate B"}

	da, db := a.Device("TEROS 12", "1.0"), b.Device("TEROS 12", "1.0")
	if da.Identifiers[0] == db.Identifiers[0] {
		t.Fatalf("both probes share device identifier %q", da.Identifiers[0])
	}
	if got := a.Topics("grow/daniel-home").Status(); got != "grow/daniel-home/substrate-a/status" {
		t.Errorf("status topic = %q", got)
	}
	if a.Topics("grow/daniel-home").Status() == b.Topics("grow/daniel-home").Status() {
		t.Error("both probes share one status topic, so one LWT would speak for both")
	}
}

func TestDeviceFallsBackToNodeIDWhenUnnamed(t *testing.T) {
	d := Probe{Address: 'A', NodeID: "substrate-a"}.Device("", "")
	if d.Name != "substrate-a" {
		t.Errorf("device name = %q, want the node id as a fallback", d.Name)
	}
	if d.Model != "TEROS 12" {
		t.Errorf("device model = %q, want a default", d.Model)
	}
}

// The table has to survive the discovery marshaller it will actually be fed to.
func TestEveryEntityRendersDiscovery(t *testing.T) {
	p := Probe{Address: 'A', NodeID: "substrate-a", Name: "Substrate A"}
	topics := p.Topics("grow/daniel-home")
	dev := p.Device("TEROS 12", "1.0")

	for _, e := range Entities() {
		payload, err := entities.DiscoveryPayload(e, topics, dev)
		if err != nil {
			t.Fatalf("%s: DiscoveryPayload: %v", e.ObjectID, err)
		}
		var got map[string]any
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s: discovery payload is not valid JSON: %v", e.ObjectID, err)
		}
		if got["unique_id"] == "" || got["unique_id"] == nil {
			t.Errorf("%s: discovery payload has no unique_id", e.ObjectID)
		}
	}
}

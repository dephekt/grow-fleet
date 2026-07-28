package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

const auxTestPPFDUnit = "µmol/s/m²"

var auxTestTopics = entities.Topics{Prefix: "grow/daniel-home", NodeID: "quantum-sensor"}

var auxTestDevice = entities.Device{
	Identifiers:  []string{"quantum-sensor"},
	Name:         deviceName,
	Manufacturer: deviceManufacturer,
	Model:        deviceModel,
	SWVersion:    "104",
}

func auxByObjectID(t *testing.T, objectID string) auxEntity {
	t.Helper()
	for _, x := range auxTable(auxTestPPFDUnit) {
		if x.ObjectID == objectID {
			return x
		}
	}
	t.Fatalf("auxiliary entity %q not found", objectID)
	return auxEntity{}
}

const auxTestDeviceJSON = `{"identifiers":["quantum-sensor"],"name":"Quantum PAR Sensor",` +
	`"manufacturer":"Apogee Instruments","model":"SQ-521","sw_version":"104"}`

// Golden payloads. Field order is fixed by the struct, so these are byte-exact
// and a reordering shows up here rather than as retained-message churn that
// wakes every subscriber on restart.
func TestAuxDiscoveryPayloadGolden(t *testing.T) {
	tests := []struct {
		objectID string
		want     string
	}{
		{
			// The reconciler's whole selector: component "text", object id
			// "time_zone", and a non-empty command_topic. Change any of the
			// three and this daemon silently stops participating in the fleet's
			// timezone push.
			"time_zone",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Time zone","object_id":"time_zone",` +
				`"unique_id":"quantum_sensor_time_zone","state_topic":"~/text/time_zone/state",` +
				`"command_topic":"~/text/time_zone/command","availability_topic":"~/status","device":` +
				auxTestDeviceJSON + `,"entity_category":"diagnostic","icon":"mdi:earth"}`,
		},
		{
			"dli",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Daily light integral","object_id":"dli",` +
				`"unique_id":"quantum_sensor_dli","state_topic":"~/sensor/dli/state",` +
				`"availability_topic":"~/status","device":` + auxTestDeviceJSON +
				`,"unit_of_measurement":"mol/m²/d","state_class":"total_increasing",` +
				`"suggested_display_precision":1,"icon":"mdi:sun-wireless"}`,
		},
		{
			// No entity_category, and that absence is the contract: grow-app's
			// recorder skips diagnostics, so a category here would stop the
			// uncredited-time figure from ever being stored beside the DLI it
			// qualifies. See TestAuxCategoriesMatchWhatGrowAppRequires.
			"dli_gap",
			`{"~":"grow/daniel-home/quantum-sensor","name":"DLI uncredited time","object_id":"dli_gap",` +
				`"unique_id":"quantum_sensor_dli_gap","state_topic":"~/sensor/dli_gap/state",` +
				`"availability_topic":"~/status","device":` + auxTestDeviceJSON +
				`,"unit_of_measurement":"min","state_class":"total_increasing",` +
				`"suggested_display_precision":1,"icon":"mdi:timer-alert-outline"}`,
		},
		{
			// The one row where the category has to be present. This unit is
			// byte-identical to the live ppfd entity's, so entity_category is
			// the only thing in this payload that stops grow-app's µmol-unit
			// fallback from resolving the day's peak as the canopy reading.
			"peak_ppfd",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Peak PPFD today","object_id":"peak_ppfd",` +
				`"unique_id":"quantum_sensor_peak_ppfd","state_topic":"~/sensor/peak_ppfd/state",` +
				`"availability_topic":"~/status","device":` + auxTestDeviceJSON +
				`,"unit_of_measurement":"` + auxTestPPFDUnit + `","state_class":"total_increasing",` +
				`"suggested_display_precision":0,"entity_category":"diagnostic",` +
				`"icon":"mdi:white-balance-sunny"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.objectID, func(t *testing.T) {
			got, err := auxByObjectID(t, tc.objectID).discoveryPayload(auxTestTopics, auxTestDevice)
			if err != nil {
				t.Fatalf("discoveryPayload: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("payload mismatch:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// The duplication guard.
//
// auxDevice mirrors an unexported struct in entities/discovery.go because
// entities.Device carries no JSON tags. A field added on one side and not the
// other would publish two different device blocks, which grow-app registers as
// two devices — the entities all still appear, just split across two cards with
// half the history on each. Comparing the bytes is the only check that catches
// it, since the types cannot be compared directly.
func TestAuxDeviceBlockMatchesEntities(t *testing.T) {
	table := entities.Build(entities.Params{PPFDUnit: auxTestPPFDUnit})
	fromEntities, err := entities.DiscoveryPayload(table[0], auxTestTopics, auxTestDevice)
	if err != nil {
		t.Fatalf("entities.DiscoveryPayload: %v", err)
	}
	fromAux, err := auxByObjectID(t, "dli").discoveryPayload(auxTestTopics, auxTestDevice)
	if err != nil {
		t.Fatalf("discoveryPayload: %v", err)
	}

	want := deviceBlock(t, fromEntities)
	got := deviceBlock(t, fromAux)
	if !bytes.Equal(got, want) {
		t.Errorf("device blocks differ; the two marshallers have drifted\n aux %s\nents %s", got, want)
	}

	// And with sw_version absent, which is a distinct code path (omitempty).
	bare := auxTestDevice
	bare.SWVersion = ""
	fromEntities, err = entities.DiscoveryPayload(table[0], auxTestTopics, bare)
	if err != nil {
		t.Fatalf("entities.DiscoveryPayload: %v", err)
	}
	fromAux, err = auxByObjectID(t, "dli").discoveryPayload(auxTestTopics, bare)
	if err != nil {
		t.Fatalf("discoveryPayload: %v", err)
	}
	if got, want := deviceBlock(t, fromAux), deviceBlock(t, fromEntities); !bytes.Equal(got, want) {
		t.Errorf("device blocks differ with sw_version unset\n aux %s\nents %s", got, want)
	}
}

// deviceBlock extracts the raw "device" value from a discovery payload.
func deviceBlock(t *testing.T, payload []byte) []byte {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal %s: %v", payload, err)
	}
	raw, ok := decoded["device"]
	if !ok {
		t.Fatalf("no device block in %s", payload)
	}
	return raw
}

// Republishing an unchanged config must be byte-identical, or every retained
// message churns on restart.
func TestAuxDiscoveryPayloadStable(t *testing.T) {
	for _, x := range auxTable(auxTestPPFDUnit) {
		first, err := x.discoveryPayload(auxTestTopics, auxTestDevice)
		if err != nil {
			t.Fatalf("%s: %v", x.ObjectID, err)
		}
		for range 8 {
			again, err := auxByObjectID(t, x.ObjectID).discoveryPayload(auxTestTopics, auxTestDevice)
			if err != nil {
				t.Fatalf("%s: %v", x.ObjectID, err)
			}
			if !bytes.Equal(first, again) {
				t.Fatalf("%s payload not stable:\n%s\n%s", x.ObjectID, first, again)
			}
		}
	}
}

// Only time_zone carries a command topic. An accidental command_topic on a
// sensor would opt it into a reconciler that has nothing to say to it.
func TestOnlyTheTimeZoneEntityIsCommandable(t *testing.T) {
	for _, x := range auxTable(auxTestPPFDUnit) {
		payload, err := x.discoveryPayload(auxTestTopics, auxTestDevice)
		if err != nil {
			t.Fatalf("%s: %v", x.ObjectID, err)
		}
		has := bytes.Contains(payload, []byte(`"command_topic"`))
		if want := x.ObjectID == timeZoneObjectID; has != want {
			t.Errorf("%s command_topic present = %v, want %v", x.ObjectID, has, want)
		}
	}
}

// The two tables share one retained map and one lastValues map, both keyed by
// object id, so a collision would make a retraction clear the wrong topic, an
// announcement skip as already-published, and a state change suppress the wrong
// publish.
func TestAuxObjectIDsDoNotCollideWithTheSensorTable(t *testing.T) {
	seen := map[string]string{}
	for _, e := range entities.Build(entities.Params{PPFDUnit: auxTestPPFDUnit}) {
		seen[e.ObjectID] = "entities table"
	}
	for _, x := range auxTable(auxTestPPFDUnit) {
		if where, ok := seen[x.ObjectID]; ok {
			t.Errorf("auxiliary entity %q collides with the %s; announced and lastValues are keyed by object id",
				x.ObjectID, where)
		}
		seen[x.ObjectID] = "auxiliary table"
	}
}

// The same rule the sensor table follows: the payload carries one more digit
// than the UI renders, so what reaches InfluxDB is not pre-rounded to today's
// dashboard formatting.
func TestAuxPayloadPrecisionFinerThanDisplay(t *testing.T) {
	for _, x := range auxTable(auxTestPPFDUnit) {
		if x.value == nil || x.DisplayPrecision == nil {
			continue
		}
		if x.Precision != *x.DisplayPrecision+1 {
			t.Errorf("%s: payload precision %d, display precision %d (want display+1)",
				x.ObjectID, x.Precision, *x.DisplayPrecision)
		}
	}
}

// grow-app substring-matches a DANGEROUS_WORDS list against ids and names and
// hides anything that matches, which is why the sensor table calls its
// multiplier "cal_factor". The rule is table-wide, not sensor-table-wide.
func TestAuxHasNoDangerousWords(t *testing.T) {
	for _, x := range auxTable(auxTestPPFDUnit) {
		hay := strings.ToLower(x.ObjectID + " " + x.Name)
		for _, word := range []string{"calibration", "calibrate"} {
			if strings.Contains(hay, word) {
				t.Errorf("auxiliary entity %q (%q) contains dangerous word %q; grow-app would hide it",
					x.ObjectID, x.Name, word)
			}
		}
	}
}

// unique_id must be node-scoped, or a bare "dli" collides with any other device
// publishing one and grow-app re-keys the loser non-deterministically on
// restart, orphaning its InfluxDB history.
func TestAuxUniqueIDsAreNodeScoped(t *testing.T) {
	for _, x := range auxTable(auxTestPPFDUnit) {
		payload, err := x.discoveryPayload(auxTestTopics, auxTestDevice)
		if err != nil {
			t.Fatalf("%s: %v", x.ObjectID, err)
		}
		var decoded struct {
			UniqueID   string `json:"unique_id"`
			Base       string `json:"~"`
			StateTopic string `json:"state_topic"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("%s: %v", x.ObjectID, err)
		}
		if !strings.HasPrefix(decoded.UniqueID, "quantum_sensor_") {
			t.Errorf("%s unique_id = %q, want a node-scoped id", x.ObjectID, decoded.UniqueID)
		}
		if strings.Contains(decoded.UniqueID, "-") {
			t.Errorf("%s unique_id = %q still contains a hyphen", x.ObjectID, decoded.UniqueID)
		}
		// The "~" shorthand has to expand to the topic the daemon actually
		// publishes to, or the config points somewhere nothing ever arrives.
		expanded := decoded.Base + strings.TrimPrefix(decoded.StateTopic, "~")
		if want := auxTestTopics.State(x.Component, x.ObjectID); expanded != want {
			t.Errorf("%s: expanded state topic %q, want %q", x.ObjectID, expanded, want)
		}
	}
}

// Build must hand out fresh pointers, for the same reason entities.Build does:
// a caller mutating *DisplayPrecision must not reach into anybody else's table.
func TestAuxTableIsFreshPerCall(t *testing.T) {
	first := auxTable(auxTestPPFDUnit)
	for i := range first {
		if first[i].DisplayPrecision != nil {
			*first[i].DisplayPrecision = 99
		}
		first[i].ObjectID = "clobbered"
	}

	second := auxTable(auxTestPPFDUnit)
	dli := auxByObjectID(t, "dli")
	if second[1].ObjectID == "clobbered" {
		t.Fatal("auxTable returns a shared slice")
	}
	if dli.DisplayPrecision == nil || *dli.DisplayPrecision != 1 {
		t.Errorf("dli display precision was mutated through a previous call's table")
	}
}

// PPFD_UNIT is operator-configurable and the peak has to carry it, or the two
// PPFD entities disagree about what they are measuring.
func TestPeakPPFDCarriesTheConfiguredUnit(t *testing.T) {
	custom := auxTable("umol/m2/s")
	for _, x := range custom {
		if x.ObjectID == "peak_ppfd" && x.Unit != "umol/m2/s" {
			t.Errorf("peak_ppfd unit = %q, want the configured PPFD unit", x.Unit)
		}
	}
}

// The categories are a wire contract with grow-app, not presentation, and each
// one is load-bearing in the opposite direction from the other. Asserted by
// name here rather than left to the golden JSON: a golden test records whatever
// the table produces, so it pins the output without ever stating the
// requirement — which is exactly how both of these shipped inverted.
func TestAuxCategoriesMatchWhatGrowAppRequires(t *testing.T) {
	for _, tc := range []struct {
		objectID string
		category string
		why      string
	}{
		{
			objectID: "peak_ppfd", category: "diagnostic",
			why: "it carries the same µmol unit as the live ppfd, and grow-app's isQuantumPpfd " +
				"falls back to that unit; without the category the peak is a candidate for " +
				"'the canopy PAR sensor' and can be anchored as one",
		},
		{
			objectID: "dli_gap", category: "",
			why: "grow-app's recorder drops diagnostic entities, and an uncredited-time figure " +
				"that is not stored beside the DLI it qualifies cannot be reconstructed later",
		},
		{
			objectID: "dli", category: "",
			why: "the measured daily total is the point of the whole feature; it has to be historised",
		},
		{
			objectID: "dli_yesterday", category: "",
			why: "same series as dli, one day back",
		},
		{
			objectID: "photoperiod", category: "",
			why: "measured lit hours; the counterpart to the planned photoperiod grow-app compares against",
		},
	} {
		t.Run(tc.objectID, func(t *testing.T) {
			x := auxByObjectID(t, tc.objectID)
			if x.Category != tc.category {
				got, want := x.Category, tc.category
				if got == "" {
					got = "(none)"
				}
				if want == "" {
					want = "(none)"
				}
				t.Errorf("%s category = %s, want %s\n  because: %s", tc.objectID, got, want, tc.why)
			}
		})
	}
}

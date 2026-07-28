package entities

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The device block as the deployed publisher sends it. sw_version comes from the
// SDI-12 I! response.
var testDevice = Device{
	Identifiers:  []string{"quantum-sensor"},
	Name:         "Quantum PAR Sensor",
	Manufacturer: "Apogee Instruments",
	Model:        "SQ-521",
	SWVersion:    "030",
}

const testDeviceJSON = `{"identifiers":["quantum-sensor"],"name":"Quantum PAR Sensor",` +
	`"manufacturer":"Apogee Instruments","model":"SQ-521","sw_version":"030"}`

// Golden discovery payloads, one per entity. Field order is fixed by the
// discoveryPayload struct, so these are byte-exact and a reordering shows up as
// a failure rather than silently churning retained messages.
func TestDiscoveryPayloadGolden(t *testing.T) {
	tests := []struct {
		objectID string
		want     string
	}{
		{
			"ppfd",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Canopy PPFD","object_id":"ppfd",` +
				`"unique_id":"quantum_sensor_ppfd","state_topic":"~/sensor/ppfd/state",` +
				`"availability_topic":"~/status","device":` + testDeviceJSON +
				`,"unit_of_measurement":"µmol/s/m²","state_class":"measurement",` +
				`"suggested_display_precision":0,"icon":"mdi:white-balance-sunny"}`,
		},
		{
			"detector_mv",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Detector signal","object_id":"detector_mv",` +
				`"unique_id":"quantum_sensor_detector_mv","state_topic":"~/sensor/detector_mv/state",` +
				`"availability_topic":"~/status","device":` + testDeviceJSON +
				`,"unit_of_measurement":"mV","state_class":"measurement",` +
				`"suggested_display_precision":3,"icon":"mdi:sine-wave"}`,
		},
		{
			"tilt",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Sensor tilt","object_id":"tilt",` +
				`"unique_id":"quantum_sensor_tilt","state_topic":"~/sensor/tilt/state",` +
				`"availability_topic":"~/status","device":` + testDeviceJSON +
				`,"unit_of_measurement":"°","state_class":"measurement",` +
				`"suggested_display_precision":1,"icon":"mdi:angle-acute"}`,
		},
		{
			// Diagnostic, no unit and no state_class: grow-history-recorder
			// skips diagnostic entities, so it is live-only by design.
			"sensor_serial",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Sensor serial","object_id":"sensor_serial",` +
				`"unique_id":"quantum_sensor_sensor_serial","state_topic":"~/sensor/sensor_serial/state",` +
				`"availability_topic":"~/status","device":` + testDeviceJSON +
				`,"entity_category":"diagnostic","icon":"mdi:identifier"}`,
		},
		{
			"cal_factor",
			`{"~":"grow/daniel-home/quantum-sensor","name":"Cal factor","object_id":"cal_factor",` +
				`"unique_id":"quantum_sensor_cal_factor","state_topic":"~/sensor/cal_factor/state",` +
				`"availability_topic":"~/status","device":` + testDeviceJSON +
				`,"suggested_display_precision":5,"entity_category":"diagnostic",` +
				`"icon":"mdi:function-variant"}`,
		},
	}

	table := Build(Params{PPFDUnit: testPPFDUnit})
	for _, tc := range tests {
		t.Run(tc.objectID, func(t *testing.T) {
			got, err := DiscoveryPayload(byObjectID(t, table, tc.objectID), testTopics, testDevice)
			if err != nil {
				t.Fatalf("DiscoveryPayload: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("payload mismatch:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// THE TRAP. ppfd's suggested_display_precision is a legitimate 0; with a plain
// int field, encoding/json's omitempty would drop the key and grow-app would
// fall back to default rendering.
func TestDisplayPrecisionZeroIsPresent(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})
	got, err := DiscoveryPayload(byObjectID(t, table, "ppfd"), testTopics, testDevice)
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}

	// Assert on the raw bytes as well as the decoded value: a future change to
	// a plain int would still decode to 0 from an absent key.
	if !bytes.Contains(got, []byte(`"suggested_display_precision":0`)) {
		t.Errorf("suggested_display_precision:0 missing from ppfd payload: %s", got)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := decoded["suggested_display_precision"]
	if !ok {
		t.Fatal("suggested_display_precision key absent; omitempty dropped the zero")
	}
	if string(raw) != "0" {
		t.Errorf("suggested_display_precision = %s, want 0", raw)
	}
}

// The other side of the pointer: an entity with no opinion on precision must
// omit the key entirely rather than assert 0.
func TestDisplayPrecisionAbsentWhenUnset(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})
	got, err := DiscoveryPayload(byObjectID(t, table, "sensor_serial"), testTopics, testDevice)
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}
	if bytes.Contains(got, []byte("suggested_display_precision")) {
		t.Errorf("sensor_serial should not carry a display precision: %s", got)
	}
}

// Republishing an unchanged config must be byte-identical, or every retained
// message churns on restart and wakes every subscriber.
func TestDiscoveryPayloadStable(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		first, err := DiscoveryPayload(e, testTopics, testDevice)
		if err != nil {
			t.Fatalf("%s: %v", e.ObjectID, err)
		}
		for i := 0; i < 8; i++ {
			// A fresh table each iteration, mimicking a process restart.
			again, err := DiscoveryPayload(
				byObjectID(t, Build(Params{PPFDUnit: testPPFDUnit}), e.ObjectID),
				testTopics, testDevice)
			if err != nil {
				t.Fatalf("%s: %v", e.ObjectID, err)
			}
			if !bytes.Equal(first, again) {
				t.Fatalf("%s payload not stable:\n%s\n%s", e.ObjectID, first, again)
			}
		}
	}
}

// Identification can fail (no I! response); the device block then simply omits
// sw_version, as the Python did by only setting the key when it parsed one.
func TestDeviceSWVersionOmittedWhenUnknown(t *testing.T) {
	dev := testDevice
	dev.SWVersion = ""

	table := Build(Params{PPFDUnit: testPPFDUnit})
	got, err := DiscoveryPayload(byObjectID(t, table, "ppfd"), testTopics, dev)
	if err != nil {
		t.Fatalf("DiscoveryPayload: %v", err)
	}
	if bytes.Contains(got, []byte("sw_version")) {
		t.Errorf("sw_version should be omitted when unknown: %s", got)
	}
	want := `"device":{"identifiers":["quantum-sensor"],"name":"Quantum PAR Sensor",` +
		`"manufacturer":"Apogee Instruments","model":"SQ-521"}`
	if !strings.Contains(string(got), want) {
		t.Errorf("device block mismatch: %s", got)
	}
}

func TestUniqueID(t *testing.T) {
	tests := []struct{ node, object, want string }{
		// Hyphens are rewritten to underscores; the id stays node-scoped so it
		// cannot collide with another device's "ppfd".
		{"quantum-sensor", "ppfd", "quantum_sensor_ppfd"},
		{"quantum-sensor", "detector_mv", "quantum_sensor_detector_mv"},
		{"quantum-sensor", "sensor_serial", "quantum_sensor_sensor_serial"},
		{"tent-a-controller", "tilt", "tent_a_controller_tilt"},
		{"nohyphen", "ppfd", "nohyphen_ppfd"},
	}
	for _, tc := range tests {
		if got := UniqueID(tc.node, tc.object); got != tc.want {
			t.Errorf("UniqueID(%q, %q) = %q, want %q", tc.node, tc.object, got, tc.want)
		}
	}
}

// unique_id must always include the node id — a bare object id collides across
// devices and grow-app re-keys the loser non-deterministically, orphaning its
// InfluxDB history.
func TestUniqueIDIsNodeScoped(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		got := UniqueID(testTopics.NodeID, e.ObjectID)
		if !strings.HasPrefix(got, "quantum_sensor_") {
			t.Errorf("%s unique_id = %q, want a node-scoped id", e.ObjectID, got)
		}
		if strings.Contains(got, "-") {
			t.Errorf("%s unique_id = %q still contains a hyphen", e.ObjectID, got)
		}
	}
}

// Discovery must be renderable for every entity, and each payload must point at
// the topic Topics.State would publish to (via the "~" shorthand).
func TestDiscoveryStateTopicMatchesTopics(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		raw, err := DiscoveryPayload(e, testTopics, testDevice)
		if err != nil {
			t.Fatalf("%s: %v", e.ObjectID, err)
		}
		var decoded struct {
			Base       string `json:"~"`
			StateTopic string `json:"state_topic"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: %v", e.ObjectID, err)
		}
		expanded := decoded.Base + strings.TrimPrefix(decoded.StateTopic, "~")
		if want := testTopics.State(e.Component, e.ObjectID); expanded != want {
			t.Errorf("%s: expanded state topic %q, want %q", e.ObjectID, expanded, want)
		}
	}
}

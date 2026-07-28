package entities

import "testing"

// The site prefix and node id actually deployed on quantum-sensor.home.arpa.
// Every golden below is a string grow-app and grow-history-recorder already have
// retained/persisted, so a diff here is a data-loss event, not a test failure.
var testTopics = Topics{Prefix: "grow/daniel-home", NodeID: "quantum-sensor"}

func TestTopicShapes(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"base", testTopics.Base(), "grow/daniel-home/quantum-sensor"},
		{"status", testTopics.Status(), "grow/daniel-home/quantum-sensor/status"},
		{
			"state",
			testTopics.State("sensor", "ppfd"),
			"grow/daniel-home/quantum-sensor/sensor/ppfd/state",
		},
		{
			"command",
			testTopics.Command("sensor", "ppfd"),
			"grow/daniel-home/quantum-sensor/sensor/ppfd/command",
		},
		{
			// node id between component and object id — grow-app's parser
			// needs this 3-segment form to resolve a nodeId.
			"discovery",
			testTopics.Discovery("sensor", "ppfd"),
			"grow/daniel-home/_discovery/sensor/quantum-sensor/ppfd/config",
		},
		{
			"discovery underscored object id",
			testTopics.Discovery("sensor", "detector_mv"),
			"grow/daniel-home/_discovery/sensor/quantum-sensor/detector_mv/config",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// Every entity in the table must resolve to the topic layout above.
func TestTopicsForEveryEntity(t *testing.T) {
	want := map[string][2]string{
		"ppfd": {
			"grow/daniel-home/quantum-sensor/sensor/ppfd/state",
			"grow/daniel-home/_discovery/sensor/quantum-sensor/ppfd/config",
		},
		"detector_mv": {
			"grow/daniel-home/quantum-sensor/sensor/detector_mv/state",
			"grow/daniel-home/_discovery/sensor/quantum-sensor/detector_mv/config",
		},
		"tilt": {
			"grow/daniel-home/quantum-sensor/sensor/tilt/state",
			"grow/daniel-home/_discovery/sensor/quantum-sensor/tilt/config",
		},
		"sensor_serial": {
			"grow/daniel-home/quantum-sensor/sensor/sensor_serial/state",
			"grow/daniel-home/_discovery/sensor/quantum-sensor/sensor_serial/config",
		},
		"cal_factor": {
			"grow/daniel-home/quantum-sensor/sensor/cal_factor/state",
			"grow/daniel-home/_discovery/sensor/quantum-sensor/cal_factor/config",
		},
	}

	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		pair, ok := want[e.ObjectID]
		if !ok {
			t.Fatalf("unexpected entity %q in table", e.ObjectID)
		}
		if got := testTopics.State(e.Component, e.ObjectID); got != pair[0] {
			t.Errorf("%s state topic = %q, want %q", e.ObjectID, got, pair[0])
		}
		if got := testTopics.Discovery(e.Component, e.ObjectID); got != pair[1] {
			t.Errorf("%s discovery topic = %q, want %q", e.ObjectID, got, pair[1])
		}
	}
}

// A node id with hyphens must survive into topics untouched — only unique_id
// rewrites them (see TestUniqueID).
func TestTopicsPreserveHyphens(t *testing.T) {
	if got := testTopics.Base(); got != "grow/daniel-home/quantum-sensor" {
		t.Errorf("hyphens mangled in base topic: %q", got)
	}
}

package entities

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

// The default PPFD_UNIT from the Python (µmol per second per square metre).
const testPPFDUnit = "µmol/s/m²"

func byObjectID(t *testing.T, table []Entity, objectID string) Entity {
	t.Helper()
	for _, e := range table {
		if e.ObjectID == objectID {
			return e
		}
	}
	t.Fatalf("entity %q not found in table", objectID)
	return Entity{}
}

// The five entities and their exact field values. These mirror the Python's
// DISCOVERY_SPECS + readings tables; a change here changes the wire contract.
func TestBuildTable(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})

	if len(table) != 5 {
		t.Fatalf("table has %d entities, want 5", len(table))
	}

	// Order matters only for deterministic publish order, but assert it anyway
	// so an accidental reshuffle is visible in review.
	wantOrder := []string{"ppfd", "detector_mv", "tilt", "sensor_serial", "cal_factor"}
	for i, want := range wantOrder {
		if table[i].ObjectID != want {
			t.Errorf("table[%d].ObjectID = %q, want %q", i, table[i].ObjectID, want)
		}
	}

	tests := []struct {
		objectID string
		want     Entity
	}{
		{"ppfd", Entity{
			ObjectID: "ppfd", Name: "Canopy PPFD", Component: "sensor",
			Unit: testPPFDUnit, StateClass: "measurement", Icon: "mdi:white-balance-sunny",
			PayloadFormat: FormatFixed, PayloadPrecision: 1,
			Kind: SourcePolled, SubCmd: "",
		}},
		{"detector_mv", Entity{
			ObjectID: "detector_mv", Name: "Detector signal", Component: "sensor",
			Unit: "mV", StateClass: "measurement", Icon: "mdi:sine-wave",
			PayloadFormat: FormatFixed, PayloadPrecision: 4,
			Kind: SourcePolled, SubCmd: "1",
		}},
		{"tilt", Entity{
			ObjectID: "tilt", Name: "Sensor tilt", Component: "sensor",
			Unit: "°", StateClass: "measurement", Icon: "mdi:angle-acute",
			PayloadFormat: FormatFixed, PayloadPrecision: 2,
			Kind: SourcePolled, SubCmd: "4", Optional: true,
		}},
		{"sensor_serial", Entity{
			ObjectID: "sensor_serial", Name: "Sensor serial", Component: "sensor",
			Icon: "mdi:identifier", Category: "diagnostic",
			PayloadFormat: FormatString,
			Kind:          SourceStatic,
		}},
		{"cal_factor", Entity{
			ObjectID: "cal_factor", Name: "Cal factor", Component: "sensor",
			Icon: "mdi:function-variant", Category: "diagnostic",
			PayloadFormat: FormatG6,
			Kind:          SourceStatic,
		}},
	}

	// Display precisions are compared separately because they are pointers.
	wantPrecision := map[string]*int{
		"ppfd":          intPtr(0),
		"detector_mv":   intPtr(3),
		"tilt":          intPtr(1),
		"sensor_serial": nil,
		"cal_factor":    intPtr(5),
	}

	for _, tc := range tests {
		t.Run(tc.objectID, func(t *testing.T) {
			got := byObjectID(t, table, tc.objectID)

			gotPrecision := got.DisplayPrecision
			got.DisplayPrecision = nil // compare the rest by value
			if got != tc.want {
				t.Errorf("entity mismatch:\n got %+v\nwant %+v", got, tc.want)
			}

			want := wantPrecision[tc.objectID]
			switch {
			case want == nil && gotPrecision != nil:
				t.Errorf("DisplayPrecision = %d, want nil", *gotPrecision)
			case want != nil && gotPrecision == nil:
				t.Errorf("DisplayPrecision = nil, want %d", *want)
			case want != nil && *gotPrecision != *want:
				t.Errorf("DisplayPrecision = %d, want %d", *gotPrecision, *want)
			}
		})
	}
}

// The payload must always carry one more digit than the UI renders, so the
// recorded value is not pre-rounded to today's dashboard formatting.
func TestPayloadPrecisionFinerThanDisplay(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		if e.PayloadFormat != FormatFixed || e.DisplayPrecision == nil {
			continue
		}
		if e.PayloadPrecision != *e.DisplayPrecision+1 {
			t.Errorf("%s: payload precision %d, display precision %d (want display+1)",
				e.ObjectID, e.PayloadPrecision, *e.DisplayPrecision)
		}
	}
}

// Every row must state its PayloadFormat explicitly. The zero value is
// formatUnset precisely so a forgotten field cannot inherit fixed-point; this
// catches the omission in the table rather than at the first publish.
func TestBuildTableSetsEveryPayloadFormat(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		if e.PayloadFormat == formatUnset {
			t.Errorf("%s has no PayloadFormat; Format would panic on its first payload", e.ObjectID)
		}
	}
}

// grow-app substring-matches a DANGEROUS_WORDS list against entity ids and
// names and hides anything that matches, so "cal_factor" must never drift back
// toward "calibration"/"calibrate".
func TestNoDangerousWords(t *testing.T) {
	dangerous := []string{"calibration", "calibrate"}
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		hay := strings.ToLower(e.ObjectID + " " + e.Name)
		for _, word := range dangerous {
			if strings.Contains(hay, word) {
				t.Errorf("entity %q (%q) contains dangerous word %q; grow-app would hide it",
					e.ObjectID, e.Name, word)
			}
		}
	}
}

// Build must hand out a fresh slice every call. The Python mutated its
// module-level DEVICE dict at runtime; that class of shared mutable state is
// what this test forbids.
func TestBuildPurity(t *testing.T) {
	first := Build(Params{PPFDUnit: testPPFDUnit})

	// Mutate every reachable part of the returned table.
	first[0].ObjectID = "clobbered"
	first[0].Name = "clobbered"
	*first[0].DisplayPrecision = 99
	first[0].DisplayPrecision = nil
	first = append(first[:1], first[2:]...) // drop detector_mv

	second := Build(Params{PPFDUnit: testPPFDUnit})
	if len(second) != 5 {
		t.Fatalf("second Build returned %d entities, want 5", len(second))
	}

	ppfd := byObjectID(t, second, "ppfd")
	if ppfd.Name != "Canopy PPFD" {
		t.Errorf("ppfd.Name = %q, want %q", ppfd.Name, "Canopy PPFD")
	}
	if ppfd.DisplayPrecision == nil {
		t.Fatal("ppfd.DisplayPrecision is nil; the first call's mutation leaked")
	}
	if *ppfd.DisplayPrecision != 0 {
		t.Errorf("ppfd display precision = %d, want 0; the pointed-to int is shared",
			*ppfd.DisplayPrecision)
	}
	byObjectID(t, second, "detector_mv") // fatals if the deletion leaked
}

// Two tables built with different params must not share state either.
func TestBuildParamIsolation(t *testing.T) {
	metric := Build(Params{PPFDUnit: testPPFDUnit})
	custom := Build(Params{PPFDUnit: "umol/m2/s"})

	if got := byObjectID(t, metric, "ppfd").Unit; got != testPPFDUnit {
		t.Errorf("first table ppfd unit = %q, want %q", got, testPPFDUnit)
	}
	if got := byObjectID(t, custom, "ppfd").Unit; got != "umol/m2/s" {
		t.Errorf("second table ppfd unit = %q, want %q", got, "umol/m2/s")
	}
	// Only ppfd is configurable; the rest are fixed by the sensor.
	if got := byObjectID(t, custom, "detector_mv").Unit; got != "mV" {
		t.Errorf("detector_mv unit = %q, want mV", got)
	}
}

func TestEligible(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})
	ppfd := byObjectID(t, table, "ppfd")
	tilt := byObjectID(t, table, "tilt")
	serial := byObjectID(t, table, "sensor_serial")
	cal := byObjectID(t, table, "cal_factor")

	tests := []struct {
		name   string
		entity Entity
		snap   Snapshot
		want   bool
	}{
		{
			"polled, nothing probed yet",
			ppfd, Snapshot{},
			true,
		},
		{
			"polled, probe came back supported",
			tilt, Snapshot{Unsupported: map[string]bool{"tilt": false}},
			true,
		},
		{
			"optional polled, definitively unsupported (pre-3033 unit)",
			tilt, Snapshot{Unsupported: map[string]bool{"tilt": true}},
			false,
		},
		{
			"polled, a different entity is unsupported",
			ppfd, Snapshot{Unsupported: map[string]bool{"tilt": true}},
			true,
		},
		{
			"static with a parsed value",
			serial, Snapshot{Statics: map[string]string{"sensor_serial": "3033"}},
			true,
		},
		{
			"static whose value failed to parse",
			serial, Snapshot{Statics: map[string]string{"cal_factor": "1.0"}},
			false,
		},
		{
			"static with a nil statics map",
			cal, Snapshot{},
			false,
		},
		{
			// An empty payload is still a payload: presence of the key, not
			// truthiness of the value, decides eligibility.
			"static with an empty but present value",
			cal, Snapshot{Statics: map[string]string{"cal_factor": ""}},
			true,
		},
		{
			// Unsupported must not be consulted for statics; they are gated by
			// Statics alone.
			"static ignores the unsupported map",
			cal, Snapshot{
				Statics:     map[string]string{"cal_factor": "1.0"},
				Unsupported: map[string]bool{"cal_factor": true},
			},
			true,
		},
		{
			// ...and Statics must not be consulted for polled entities.
			"polled ignores the statics map",
			ppfd, Snapshot{Statics: map[string]string{}},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entity.Eligible(tc.snap); got != tc.want {
				t.Errorf("Eligible() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Optional is what separates "may legitimately be absent" from "must always be
// there", so Unsupported must only be able to switch off entities that carry it.
//
// Without this, a generically-written prober ("the aM! header advertised zero
// values, so mark it unsupported") that hits one garbled 0M! header permanently
// disables the primary PPFD entity: no discovery, no polling, no recovery short
// of a restart. A transient line-noise event must never be able to retire a
// required entity.
func TestEligibleIgnoresUnsupportedForRequiredEntities(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})

	for _, e := range table {
		if e.Kind != SourcePolled || e.Optional {
			continue
		}
		snap := Snapshot{Unsupported: map[string]bool{e.ObjectID: true}}
		if !e.Eligible(snap) {
			t.Errorf("%s is not Optional but Unsupported[%s]=true made it ineligible; "+
				"one bad probe would take the entity off the bus permanently",
				e.ObjectID, e.ObjectID)
		}
	}

	// The converse: the entity that *is* Optional must still be gated off, or
	// the flag means nothing in the other direction either.
	tilt := byObjectID(t, table, "tilt")
	if !tilt.Optional {
		t.Fatal("tilt is expected to be the Optional entity; this test needs rewriting")
	}
	if tilt.Eligible(Snapshot{Unsupported: map[string]bool{"tilt": true}}) {
		t.Error("tilt is Optional and probed unsupported, but Eligible() = true")
	}
}

// Per-entity expectations for one realistic post-probe snapshot: a pre-3033 unit
// whose V! read failed. This is a table-wide spot check of the predicate, not a
// statement about the two call sites — see TestEligibleSetDrivesBothPaths for
// that.
func TestEligibleAcrossTheTable(t *testing.T) {
	snap := Snapshot{
		Statics:     map[string]string{"sensor_serial": "3033"},
		Unsupported: map[string]bool{"tilt": true},
	}

	want := map[string]bool{
		"ppfd":          true,
		"detector_mv":   true,
		"tilt":          false, // probed absent
		"sensor_serial": true,
		"cal_factor":    false, // V! read failed, so nothing to announce
	}

	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		if got := e.Eligible(snap); got != want[e.ObjectID] {
			t.Errorf("%s: Eligible() = %v, want %v", e.ObjectID, got, want[e.ObjectID])
		}
	}
}

// The whole point of EligibleSet is that the app derives ONE working set from
// ONE snapshot and uses it for both discovery and servicing. This drives both
// paths off it and asserts they cover exactly the same entities: nothing gets a
// retained config whose state topic never receives a value, and nothing gets
// serviced that was never announced.
func TestEligibleSetDrivesBothPaths(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})
	snap := Snapshot{
		Statics:     map[string]string{"sensor_serial": "3033", "cal_factor": "1.0"},
		Unsupported: map[string]bool{"tilt": true},
	}

	working := EligibleSet(table, snap)

	// Path 1: the discovery pass announces one retained config per entity.
	announced := map[string]bool{}
	for _, e := range working {
		announced[e.ObjectID] = true
	}

	// Path 2: the servicing pass. A polled entity gets a value every cycle; a
	// static gets its parsed payload republished. An entity that reaches this
	// loop with no way to produce a payload is precisely the defect the shared
	// set exists to prevent.
	serviced := map[string]bool{}
	for _, e := range working {
		switch e.Kind {
		case SourcePolled:
			serviced[e.ObjectID] = true
		case SourceStatic:
			if _, ok := snap.Statics[e.ObjectID]; !ok {
				t.Errorf("%s is in the working set but has no parsed payload; "+
					"its retained config would point at a state topic that never fills",
					e.ObjectID)
				continue
			}
			serviced[e.ObjectID] = true
		}
	}

	if !maps.Equal(announced, serviced) {
		t.Errorf("discovery and servicing disagree:\nannounced %v\nserviced  %v", announced, serviced)
	}

	// The helper must not drift from the predicate it is supposed to apply.
	for _, e := range table {
		if got, want := announced[e.ObjectID], e.Eligible(snap); got != want {
			t.Errorf("%s: EligibleSet membership = %v, Eligible() = %v", e.ObjectID, got, want)
		}
	}

	// tilt is the case that motivates all of this: probed absent, so it must be
	// in neither path.
	if announced["tilt"] || serviced["tilt"] {
		t.Error("tilt was probed unsupported but still reached a publish path")
	}
}

// EligibleSet must not filter in place: the caller's table is reused across
// reconnects and a later, more permissive snapshot has to see every row again.
func TestEligibleSetDoesNotAliasTheTable(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})

	strict := EligibleSet(table, Snapshot{Unsupported: map[string]bool{"tilt": true}})
	if len(strict) != 2 { // ppfd and detector_mv; tilt probed absent, statics unparsed
		t.Fatalf("EligibleSet returned %d entities, want 2", len(strict))
	}

	// Mutating the result must not reach the table.
	strict[0].ObjectID = "clobbered"

	if len(table) != 5 {
		t.Fatalf("table has %d entities after EligibleSet, want 5", len(table))
	}
	wantOrder := []string{"ppfd", "detector_mv", "tilt", "sensor_serial", "cal_factor"}
	for i, want := range wantOrder {
		if table[i].ObjectID != want {
			t.Errorf("table[%d].ObjectID = %q, want %q; EligibleSet wrote through to the caller's slice",
				i, table[i].ObjectID, want)
		}
	}

	// A later snapshot that supports everything must still see all five rows.
	full := EligibleSet(table, Snapshot{
		Statics: map[string]string{"sensor_serial": "3033", "cal_factor": "1.0"},
	})
	if len(full) != 5 {
		t.Errorf("second EligibleSet returned %d entities, want 5", len(full))
	}
}

// Snapshot.Statics is keyed by ObjectID and by nothing else.
//
// Entity used to carry a StaticID field naming "the startup-parsed value that
// feeds this entity". Eligible never read it — it keys Statics by ObjectID — and
// because both statics happened to have StaticID == ObjectID the divergence was
// invisible. A producer that populated Statics by StaticID, the natural reading
// of the field name, would get Eligible == false forever and an entity that is
// never announced, silently. The field is deleted; this test keeps it deleted,
// and fails loudly if any other second identifier is bolted onto Entity.
func TestEntityHasNoIDFieldShadowingObjectID(t *testing.T) {
	typ := reflect.TypeOf(Entity{})
	for _, f := range reflect.VisibleFields(typ) {
		if f.Name == "ObjectID" || f.Type.Kind() != reflect.String {
			continue
		}
		if strings.HasSuffix(f.Name, "ID") {
			t.Errorf("Entity.%s is a second string id alongside ObjectID; "+
				"Snapshot.Statics is ObjectID-keyed, so a producer that keys by %s "+
				"silently gets entities that are never announced",
				f.Name, f.Name)
		}
	}
}

// The positive statement of the same rule: a Statics map keyed by ObjectID is
// what makes a static entity eligible.
func TestEligibleKeysStaticsByObjectID(t *testing.T) {
	for _, e := range Build(Params{PPFDUnit: testPPFDUnit}) {
		if e.Kind != SourceStatic {
			continue
		}
		if !e.Eligible(Snapshot{Statics: map[string]string{e.ObjectID: "x"}}) {
			t.Errorf("%s: a Statics entry under its ObjectID did not make it eligible", e.ObjectID)
		}
		if e.Eligible(Snapshot{Statics: map[string]string{e.ObjectID + "_other": "x"}}) {
			t.Errorf("%s: a Statics entry under a different key made it eligible", e.ObjectID)
		}
	}
}

func intPtr(n int) *int { return &n }

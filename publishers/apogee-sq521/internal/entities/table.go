package entities

// SourceKind says where an entity's value comes from, which decides both how it
// is produced and how eligibility is determined (see Entity.Eligible).
type SourceKind uint8

const (
	// SourcePolled values are re-read from the sensor every poll cycle via an
	// SDI-12 measurement command.
	SourcePolled SourceKind = iota
	// SourceStatic values are read once at startup (identification / verify)
	// and republished on every reconnect.
	SourceStatic
)

// FormatKind selects the wire representation of a value. It is deliberately
// separate from DisplayPrecision: the payload carries more resolution than the
// UI shows (see Entity.PayloadPrecision).
type FormatKind uint8

const (
	// formatUnset is the zero value and is deliberately not a usable format: a
	// row added to the table that forgets PayloadFormat would otherwise inherit
	// whichever kind happened to be first and render plausible-looking wrong
	// payloads. Format panics on it instead. It is unexported because no caller
	// has a reason to name it — only to avoid leaving it set.
	formatUnset FormatKind = iota
	// FormatFixed renders a float with PayloadPrecision decimal places.
	FormatFixed
	// FormatG6 renders with %.6g, matching the Python's cal_factor formatting.
	FormatG6
	// FormatString marks an entity whose payload is already a string (it is
	// carried verbatim in Snapshot.Statics and never goes through Format).
	FormatString
)

// Entity is one row of the table: everything needed to announce the entity over
// discovery *and* to obtain its value. Discovery-only or polling-only fields
// living in the same struct is the point — the two can no longer drift.
type Entity struct {
	ObjectID   string
	Name       string
	Component  string // MQTT discovery component; "sensor" for all of these
	Unit       string
	StateClass string
	Icon       string
	Category   string // "diagnostic" or "" (diagnostic entities are live-only, not historised)

	// DisplayPrecision becomes suggested_display_precision. It is a pointer
	// because 0 is a meaningful value for ppfd and `omitempty` on a plain int
	// would silently drop it; see discovery.go for the full explanation.
	DisplayPrecision *int
	PayloadFormat    FormatKind
	// PayloadPrecision is intentionally one digit finer than DisplayPrecision
	// so the value on the wire (and therefore in InfluxDB) is not pre-rounded
	// to whatever the dashboard happens to render today.
	PayloadPrecision int

	Kind SourceKind
	// SubCmd is the SDI-12 measurement sub-command for polled entities:
	// "" => aM!, "1" => aM1!, "4" => aM4! (Apogee SQ-521 manual, address a).
	SubCmd string
	// Index selects which value of the aD0! data response to take. The SQ-521
	// returns a single value per measurement command, so this is 0 throughout;
	// the field exists so a multi-value command needs no new plumbing.
	Index int
	// Optional marks a polled entity the sensor may not implement — tilt only
	// exists on SQ-521 units with serial >= 3033. It is capability-probed once
	// and then recorded in Snapshot.Unsupported rather than retried forever.
	Optional bool
}

// Params carries the config-dependent bits of the table so Build can stay a pure
// function of its input.
type Params struct{ PPFDUnit string }

// Build returns a freshly allocated table. It must never hand out a shared
// package-level slice: PPFDUnit is user-configurable, and the Python's
// module-level DEVICE dict — mutated at runtime to stash sw_version — is exactly
// the pattern being avoided. Callers may sort, filter or edit the result without
// affecting anyone else, including the int values behind DisplayPrecision.
func Build(p Params) []Entity {
	// precision returns a fresh *int per call so that a caller mutating
	// *e.DisplayPrecision cannot reach into a later Build's table.
	precision := func(n int) *int { return &n }

	return []Entity{
		{
			ObjectID:         "ppfd",
			Name:             "Canopy PPFD",
			Component:        "sensor",
			Unit:             p.PPFDUnit,
			StateClass:       "measurement",
			Icon:             "mdi:white-balance-sunny",
			DisplayPrecision: precision(0), // whole µmol; the payload keeps a decimal
			PayloadFormat:    FormatFixed,
			PayloadPrecision: 1,
			Kind:             SourcePolled,
			SubCmd:           "", // 0M! / 0D0!
		},
		{
			ObjectID:         "detector_mv",
			Name:             "Detector signal",
			Component:        "sensor",
			Unit:             "mV",
			StateClass:       "measurement",
			Icon:             "mdi:sine-wave",
			DisplayPrecision: precision(3),
			PayloadFormat:    FormatFixed,
			PayloadPrecision: 4,
			Kind:             SourcePolled,
			SubCmd:           "1", // 0M1! / 0D0!
		},
		{
			ObjectID:         "tilt",
			Name:             "Sensor tilt",
			Component:        "sensor",
			Unit:             "°",
			StateClass:       "measurement",
			Icon:             "mdi:angle-acute",
			DisplayPrecision: precision(1),
			PayloadFormat:    FormatFixed,
			PayloadPrecision: 2,
			Kind:             SourcePolled,
			SubCmd:           "4", // 0M4! / 0D0!; absent on pre-3033 units
			Optional:         true,
		},
		{
			ObjectID:      "sensor_serial",
			Name:          "Sensor serial",
			Component:     "sensor",
			Icon:          "mdi:identifier",
			Category:      "diagnostic",
			PayloadFormat: FormatString,
			Kind:          SourceStatic,
		},
		{
			// Do NOT rename this to anything containing "calibration" or
			// "calibrate": grow-app substring-matches those against a
			// DANGEROUS_WORDS list and excludes matching entities from the
			// dashboard entirely.
			ObjectID:         "cal_factor",
			Name:             "Cal factor",
			Component:        "sensor",
			Icon:             "mdi:function-variant",
			Category:         "diagnostic",
			DisplayPrecision: precision(5),
			PayloadFormat:    FormatG6,
			Kind:             SourceStatic,
		},
	}
}

// Snapshot is what startup discovered about this particular sensor: the values
// that were actually parsed and the capabilities that were definitively probed.
//
// # Ordering contract
//
// A Snapshot carries no notion of "not probed yet". An empty Unsupported map
// means "everything is supported", not "nothing has been asked" — the two are
// indistinguishable here and this package cannot tell them apart. Eligible
// therefore only keeps a discovery config off the bus if the probe already ran.
//
// Callers must, in this order:
//
//  1. run identification (I!/V!) and the capability probe,
//  2. build one Snapshot from the results,
//  3. resolve the working set once with EligibleSet, and only then
//  4. publish discovery and start servicing, both from that one set.
//
// Publishing discovery earlier — at MQTT connect, as the Python did in
// on_connect — hands a zero Snapshot to Eligible, which announces tilt on a
// pre-3033 unit; the first poll then marks it unsupported and the entity shows
// up forever as permanently unknown.
//
// Retained configs do not clear themselves. If a later probe flips Unsupported
// mid-run, recompute EligibleSet against the new Snapshot, diff it against the
// previously published set, and retract what dropped out by publishing an empty
// retained message to Topics.Discovery for each. Nothing in this package does
// that for you.
type Snapshot struct {
	// SWVersion is the firmware string from the SDI-12 I! response, used to
	// populate the discovery device block. Empty when identification failed.
	SWVersion string
	// Statics maps objectID to an already-formatted payload. A key is present
	// only if the value was genuinely parsed — never as an empty placeholder.
	Statics map[string]string
	// Unsupported maps objectID to true only after a definitive capability
	// probe (the sensor advertised zero values for the command). A failed read
	// that might be transient must not land here.
	Unsupported map[string]bool
}

// EligibleSet resolves the whole table against one Snapshot and returns the
// entities to announce and to service. Prefer it over calling Eligible per
// entity at each site: the two call sites cannot drift if there is only one
// list, and the list is then also the thing to diff against on the next probe
// to find retained configs that need retracting.
//
// Call it after the capability probe, not before — see the ordering contract on
// Snapshot. The result is a fresh slice; the input table is untouched.
func EligibleSet(table []Entity, s Snapshot) []Entity {
	working := make([]Entity, 0, len(table))
	for _, e := range table {
		if e.Eligible(s) {
			working = append(working, e)
		}
	}
	return working
}

// Eligible reports whether this entity should be both announced and serviced.
//
// One predicate gates discovery publication and servicing together, so an entity
// cannot be announced without a value source or serviced without being
// announced — provided both are derived from the same post-probe Snapshot, which
// is what EligibleSet is for. Nothing here can enforce that ordering; see the
// contract documented on Snapshot.
func (e Entity) Eligible(s Snapshot) bool {
	if e.Kind == SourceStatic {
		// No parsed value means nothing to publish, so nothing to announce.
		_, ok := s.Statics[e.ObjectID]
		return ok
	}
	// Only an Optional entity can be probed away. A required entity — ppfd is
	// the whole point of the device — stays eligible no matter what lands in
	// Unsupported, so a prober that mis-reads one garbled measurement header
	// cannot take the primary reading off the bus until the next restart.
	// Reading a nil map is fine and yields false.
	return !e.Optional || !s.Unsupported[e.ObjectID]
}

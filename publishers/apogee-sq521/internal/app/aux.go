package app

import (
	"encoding/json"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// The auxiliary table: entities this daemon publishes that do not come from the
// sensor.
//
// # Why they are not in entities.Build
//
// Two independent reasons, both structural rather than stylistic:
//
//   - The time_zone entity needs a command_topic. grow-app's timezone
//     reconciler selects on component == "text" && objectId == "time_zone" &&
//     commandTopic != "", and entities.DiscoveryPayload has no such field —
//     every entity it describes is read-only, which is true of everything the
//     SQ-521 exposes. An entity that cannot be expressed by that marshaller
//     cannot live in that table.
//
//   - The DLI entities have no value source that entities.Snapshot can
//     describe. Snapshot is defined entirely in terms of one SDI-12 probe:
//     Statics are values parsed from 0I!/0V!, Unsupported records commands the
//     sensor declined, and Entity carries a SubCmd and an Index into a data
//     response. A derived quantity integrated over a local day has none of
//     those, and Eligible would have nothing to decide.
//
// What is *not* a reason: convenience. The invariant the entities package
// exists to protect — that an entity cannot be announced without a value source
// or serviced without being announced — is preserved here by the same means,
// one struct carrying both halves. auxEntity.value is what produces the
// payload, and there is no path that publishes a config for a row without it
// except the one row that is deliberately written from elsewhere (time_zone,
// whose value comes from the Zones manager rather than from a dli.Reading).
//
// Constraint respected throughout: no entities.Entity is constructed here.
// entities.Format panics on an unset FormatKind by design, so the only safe
// source of an Entity is entities.Build.

// auxEntity is one row of the auxiliary table.
type auxEntity struct {
	ObjectID  string
	Name      string
	Component string
	Unit      string
	// StateClass is the Home-Assistant-style hint grow-app records alongside
	// the value. "total_increasing" is the right one for anything that climbs
	// through a day and resets at local midnight; the DLI accumulator does
	// exactly that, and a consumer told "measurement" would draw the midnight
	// reset as a real cliff.
	StateClass string
	Icon       string
	// Category is "diagnostic" for rows that are live-only and not historised.
	Category string

	// DisplayPrecision becomes suggested_display_precision. Pointer for the
	// same reason entities.Entity uses one: a legitimate 0 must not be dropped
	// by omitempty.
	DisplayPrecision *int
	// Precision is the number of decimals on the wire, deliberately one finer
	// than DisplayPrecision so the recorded value is not pre-rounded to
	// whatever the dashboard renders today.
	Precision int

	// Command makes the discovery config carry a command_topic, which is what
	// opts an entity into grow-app's reconcilers. Only time_zone sets it.
	Command bool

	// value extracts this row's payload from a DLI reading. Nil means the value
	// comes from somewhere else entirely (time_zone), and the row is skipped by
	// the DLI publish pass.
	value func(dli.Reading) float64
}

// timeZoneObjectID is the object id grow-app's timezone reconciler selects on,
// alongside component "text" and a non-empty command topic. It is wire
// contract: rename it and the daemon silently stops participating.
const timeZoneObjectID = "time_zone"

// timeZoneComponent is the other half of that selector.
const timeZoneComponent = "text"

// auxTable returns a freshly allocated auxiliary table. Fresh for the same
// reason entities.Build is: the DisplayPrecision pointers must not be shared
// between callers.
func auxTable(ppfdUnit string) []auxEntity {
	precision := func(n int) *int { return &n }

	return []auxEntity{
		{
			// The reconciler's target. Diagnostic because it is configuration
			// rather than measurement — there is nothing to historise about a
			// zone that changes twice a decade.
			ObjectID:  timeZoneObjectID,
			Name:      "Time zone",
			Component: timeZoneComponent,
			Icon:      "mdi:earth",
			Category:  "diagnostic",
			Command:   true,
		},
		{
			ObjectID:         "dli",
			Name:             "Daily light integral",
			Component:        "sensor",
			Unit:             "mol/m²/d",
			StateClass:       "total_increasing",
			Icon:             "mdi:sun-wireless",
			DisplayPrecision: precision(1),
			Precision:        2,
			value:            func(r dli.Reading) float64 { return r.DLI },
		},
		{
			// A completed total that changes once a day: it does not climb, so
			// it is a measurement rather than an increasing total.
			ObjectID:         "dli_yesterday",
			Name:             "DLI yesterday",
			Component:        "sensor",
			Unit:             "mol/m²/d",
			StateClass:       "measurement",
			Icon:             "mdi:weather-sunset-down",
			DisplayPrecision: precision(1),
			Precision:        2,
			value:            func(r dli.Reading) float64 { return r.Yesterday },
		},
		{
			ObjectID:         "photoperiod",
			Name:             "Photoperiod today",
			Component:        "sensor",
			Unit:             "h",
			StateClass:       "total_increasing",
			Icon:             "mdi:sun-clock",
			DisplayPrecision: precision(2),
			Precision:        3,
			value:            func(r dli.Reading) float64 { return r.PhotoperiodHours },
		},
		{
			ObjectID:         "peak_ppfd",
			Name:             "Peak PPFD today",
			Component:        "sensor",
			Unit:             ppfdUnit,
			StateClass:       "total_increasing",
			Icon:             "mdi:white-balance-sunny",
			DisplayPrecision: precision(0),
			Precision:        1,
			value:            func(r dli.Reading) float64 { return r.PeakPPFD },
		},
		{
			// Data quality, not measurement: minutes of today that went
			// uncredited because samples were missing. A non-zero value means
			// the DLI above under-reports by an unknown amount, which is the
			// one thing a grower needs to know before trusting it.
			ObjectID:         "dli_gap",
			Name:             "DLI uncredited time",
			Component:        "sensor",
			Unit:             "min",
			StateClass:       "total_increasing",
			Icon:             "mdi:timer-alert-outline",
			Category:         "diagnostic",
			DisplayPrecision: precision(1),
			Precision:        2,
			value:            func(r dli.Reading) float64 { return r.GapMinutes },
		},
	}
}

// auxDevice mirrors the unexported marshalling view in entities/discovery.go.
//
// It is duplicated rather than shared because entities.Device carries no JSON
// tags — the tagged struct is unexported, which is right for that package and
// leaves this one with no way to reuse it. The duplication is pinned by
// TestAuxDeviceBlockMatchesEntities, which compares the bytes the two
// marshallers produce for the same Device; a field added on one side and not
// the other fails there rather than shipping two device blocks that grow-app
// registers as two devices.
type auxDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	SWVersion    string   `json:"sw_version,omitempty"`
}

// auxPayload is entities' discoveryPayload plus command_topic.
//
// Field order is declaration order, so the bytes are deterministic — which is
// what lets App.announce compare a rendered config against the one it last put
// on the broker and skip the publish when nothing changed. That comparison is
// the point: a retained PUBLISH is forwarded to every current subscriber
// whether or not the payload differs from the retained one it replaces, so
// republishing an identical config is not free on the subscriber side. It wakes
// grow-app for no information, and a reopen loop does it several times a
// minute.
type auxPayload struct {
	Base       string `json:"~"`
	Name       string `json:"name"`
	ObjectID   string `json:"object_id"`
	UniqueID   string `json:"unique_id"`
	StateTopic string `json:"state_topic"`
	// Omitted for everything except time_zone. Its presence is what grow-app's
	// reconciler selects on, so it is not decoration.
	CommandTopic      string    `json:"command_topic,omitempty"`
	AvailabilityTopic string    `json:"availability_topic"`
	Device            auxDevice `json:"device"`
	Unit              string    `json:"unit_of_measurement,omitempty"`
	StateClass        string    `json:"state_class,omitempty"`
	DisplayPrecision  *int      `json:"suggested_display_precision,omitempty"`
	EntityCategory    string    `json:"entity_category,omitempty"`
	Icon              string    `json:"icon,omitempty"`
}

// discoveryPayload renders the retained config message for one auxiliary entity,
// using the same "~" shorthand as entities.DiscoveryPayload so the two families
// of retained payload stay comparable by eye.
func (a auxEntity) discoveryPayload(t entities.Topics, dev entities.Device) ([]byte, error) {
	p := auxPayload{
		Base:              t.Base(),
		Name:              a.Name,
		ObjectID:          a.ObjectID,
		UniqueID:          entities.UniqueID(t.NodeID, a.ObjectID),
		StateTopic:        "~/" + a.Component + "/" + a.ObjectID + "/state",
		AvailabilityTopic: "~/status",
		Device: auxDevice{
			Identifiers:  dev.Identifiers,
			Name:         dev.Name,
			Manufacturer: dev.Manufacturer,
			Model:        dev.Model,
			SWVersion:    dev.SWVersion,
		},
		Unit:             a.Unit,
		StateClass:       a.StateClass,
		DisplayPrecision: a.DisplayPrecision,
		EntityCategory:   a.Category,
		Icon:             a.Icon,
	}
	if a.Command {
		p.CommandTopic = "~/" + a.Component + "/" + a.ObjectID + "/command"
	}
	return json.Marshal(p)
}

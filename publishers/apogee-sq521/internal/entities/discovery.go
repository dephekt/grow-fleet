package entities

import (
	"encoding/json"
	"strings"
)

// Device is the shared device block. Every entity repeats it so grow-app groups
// them into one device card instead of five orphan sensors.
type Device struct {
	Identifiers  []string
	Name         string
	Manufacturer string
	Model        string
	SWVersion    string
}

// device is the marshalling view of Device: explicit tags, explicit order.
type device struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	// Omitted when identification failed, matching the Python which only set
	// DEVICE["sw_version"] if it managed to parse a firmware string.
	SWVersion string `json:"sw_version,omitempty"`
}

// discoveryPayload is a struct rather than a map on purpose. Go marshals struct
// fields in declaration order, so republishing an unchanged config produces
// byte-identical output; a map would emit keys in sorted order but any future
// change of key set reshuffles everything, and retained-message churn wakes up
// every subscriber for no reason.
type discoveryPayload struct {
	Base              string `json:"~"`
	Name              string `json:"name"`
	ObjectID          string `json:"object_id"`
	UniqueID          string `json:"unique_id"`
	StateTopic        string `json:"state_topic"`
	AvailabilityTopic string `json:"availability_topic"`
	Device            device `json:"device"`
	Unit              string `json:"unit_of_measurement,omitempty"`
	StateClass        string `json:"state_class,omitempty"`
	// Pointer, not int. THE TRAP: with a plain int, `omitempty` treats ppfd's
	// legitimate suggested_display_precision of 0 as "unset" and drops the key,
	// at which point grow-app falls back to its default rendering and PPFD
	// shows spurious decimals. A nil pointer means "no opinion"; a pointer to
	// zero means "render whole numbers".
	DisplayPrecision *int   `json:"suggested_display_precision,omitempty"`
	EntityCategory   string `json:"entity_category,omitempty"`
	Icon             string `json:"icon,omitempty"`
}

// DiscoveryPayload renders the retained config message for one entity.
func DiscoveryPayload(e Entity, t Topics, dev Device) ([]byte, error) {
	return json.Marshal(discoveryPayload{
		Base:     t.Base(),
		Name:     e.Name,
		ObjectID: e.ObjectID,
		UniqueID: UniqueID(t.NodeID, e.ObjectID),
		// Written through the "~" shorthand exactly as the Python does, so the
		// retained payloads stay comparable across the port.
		StateTopic:        "~/" + e.Component + "/" + e.ObjectID + "/state",
		AvailabilityTopic: "~/status",
		Device: device{
			Identifiers:  dev.Identifiers,
			Name:         dev.Name,
			Manufacturer: dev.Manufacturer,
			Model:        dev.Model,
			SWVersion:    dev.SWVersion,
		},
		Unit:             e.Unit,
		StateClass:       e.StateClass,
		DisplayPrecision: e.DisplayPrecision,
		EntityCategory:   e.Category,
		Icon:             e.Icon,
	})
}

// UniqueID scopes the entity id to the node.
//
// Node-scoping is not cosmetic: a bare "ppfd" collides with any other device
// publishing the same object id, and grow-app then re-keys the loser
// non-deterministically on restart, which orphans its InfluxDB history. Hyphens
// are replaced because downstream identifiers are used in contexts where "-"
// is not safe.
func UniqueID(nodeID, objectID string) string {
	return strings.ReplaceAll(nodeID+"_"+objectID, "-", "_")
}

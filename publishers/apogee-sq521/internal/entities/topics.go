// Package entities holds the single declarative table describing everything this
// publisher puts on the MQTT bus, plus the topic and payload shapes derived from
// it.
//
// The Python original kept two parallel tables — DISCOVERY_SPECS (what to
// announce) and `readings` (what to poll) — with the display precision split
// across them. Renaming an object id in one and not the other produced a
// retained discovery config whose state topic never received a value, which
// grow-app happily registers and then shows as a permanently-unknown entity.
// Everything here is derived from one []Entity so that class of bug cannot be
// expressed.
//
// Every string this package produces is part of a wire contract:
// grow-history-recorder keys InfluxDB series on the node id, object id and
// topic layout, so changing any of them orphans history rather than migrating
// it. Treat the golden tests as the specification.
package entities

// Topics renders every topic this publisher touches from the site prefix and the
// node id. Prefix is a multi-segment site root such as "grow/daniel-home";
// NodeID is the device identity ("quantum-sensor") that also appears in each
// entity's unique_id.
type Topics struct{ Prefix, NodeID string }

// Base is the device root, published as the discovery "~" shorthand so state and
// availability topics can be written relative to it.
func (t Topics) Base() string { return t.Prefix + "/" + t.NodeID }

// Status is the availability topic: retained "online" while connected, and the
// LWT publishes "offline" here if the broker loses us.
func (t Topics) Status() string { return t.Base() + "/status" }

// State is where a value for one entity is published (retained, so a subscriber
// that connects late still sees the last reading).
func (t Topics) State(component, objectID string) string {
	return t.Base() + "/" + component + "/" + objectID + "/state"
}

// Command is the inbound counterpart to State. Nothing on the SQ-521 is
// writable, but the shape belongs here so it stays consistent with the ESPHome
// fleet if a controllable entity is ever added.
func (t Topics) Command(component, objectID string) string {
	return t.Base() + "/" + component + "/" + objectID + "/command"
}

// Discovery is the retained config topic for one entity.
//
// The node id sits *between* the component and the object id
// (<prefix>/_discovery/<component>/<node>/<object>/config). grow-app's topic
// parser requires exactly that 3-segment form after the discovery prefix to
// resolve a nodeId at all; the flatter Home Assistant style
// (<component>/<object>/config) parses to an empty node and the entity is
// dropped on the floor.
func (t Topics) Discovery(component, objectID string) string {
	return t.Prefix + "/_discovery/" + component + "/" + t.NodeID + "/" + objectID + "/config"
}

// Package substrate reads METER TEROS 11/12 soil probes sharing the SDI-12 bus
// with the Apogee quantum sensor, and describes them as MQTT entities.
//
// It publishes RAW sensor output only — calibrated counts, temperature and bulk
// EC, exactly the three fields aD0! returns. It deliberately does NOT convert
// counts to volumetric water content. That conversion is a property of the
// SUBSTRATE, not of the sensor: coco, peat, perlite and mineral soil each need a
// different equation, and the substrate a probe sits in is recorded per zone in
// grow-app, not here. Baking one curve into this binary would make changing a
// zone's medium a firmware redeploy, and would destroy the raw counts on their
// way into InfluxDB where no later correction could recover them.
//
// The same reasoning excludes pore-water EC: its Hilhorst offset is
// substrate-dependent too.
//
// This package owns no I/O policy. It is handed an [sdi12.Client] already
// addressed to one probe and returns readings; the caller decides when to poll,
// where to publish, and what to do about failure.
package substrate

import (
	"fmt"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// Object ids. Every string here is a wire contract: grow-history-recorder keys
// InfluxDB series on (node id, object id), so renaming one orphans that series'
// history rather than migrating it.
//
// The "substrate_" prefix is load-bearing beyond tidiness. grow-app's
// isAmbientTemperature rejects soil probes by matching substrate|soil|medium|root
// in the object id; without the prefix a probe reporting °C from inside a pot can
// win the CLIMATE card whenever the air rig is late to discovery.
//
// No id may contain "calibration", "calibrate" or "reset": grow-app's
// isDangerousEntity substring-matches those and filters the entity out of the
// dashboard's fallback metrics, which would make a probe silently invisible.
const (
	ObjectRawCounts   = "substrate_raw_counts"
	ObjectTemperature = "substrate_temperature"
	ObjectBulkEC      = "substrate_bulk_ec"
	ObjectSerial      = "substrate_serial"
)

// Units. Bulk EC is published in mS/cm although the sensor reports whole µS/cm,
// because every grower-facing EC number in this system is mS/cm. The conversion
// is a plain ÷1000 with no substrate term in it, so it is a unit change rather
// than a calibration.
const (
	UnitTemperature = "°C"
	UnitBulkEC      = "mS/cm"
)

// Value indices within the aD0! response: a+<counts>±<tempC>+<bulkEC µS/cm>.
// electricalConductivity is TEROS 12 only; a TEROS 11 returns two values and the
// bulk-EC entity is dropped rather than published as a fabricated zero.
const (
	idxRawCounts   = 0
	idxTemperature = 1
	idxBulkEC      = 2
)

// microsiemensPerMillisiemens converts the sensor's native µS/cm to published
// mS/cm.
const microsiemensPerMillisiemens = 1000.0

// Probe is one TEROS on the bus: where to find it, and who it is on MQTT.
type Probe struct {
	// Address is its SDI-12 address. Probes live on letters (A–D) because the
	// numeric range is kept clear as an arrivals area for factory-fresh sensors,
	// which ship on 0 or 1 and would otherwise collide with an installed probe.
	Address byte

	// NodeID is the MQTT device identity, e.g. "substrate-a". It must use
	// hyphens: grow-app's normalizeDiscoveryId collapses '-' to '_' when
	// deriving entity ids, so "substrate-a" and "substrate_a" would produce
	// colliding ids for two different devices.
	NodeID string

	// Name is the human label on the device card.
	Name string
}

// Validate reports whether the probe is addressable and safely named.
func (p Probe) Validate() error {
	if !validAddress(p.Address) {
		return fmt.Errorf("substrate: address %q must be one character from [0-9a-zA-Z]", p.Address)
	}
	if p.NodeID == "" {
		return fmt.Errorf("substrate: probe at address %q has no node id", p.Address)
	}
	for i := 0; i < len(p.NodeID); i++ {
		if p.NodeID[i] == '_' {
			return fmt.Errorf(
				"substrate: node id %q must use '-' not '_': grow-app collapses '-' to '_' when "+
					"deriving entity ids, so both spellings would collide", p.NodeID)
		}
	}
	return nil
}

func validAddress(a byte) bool {
	return (a >= '0' && a <= '9') || (a >= 'a' && a <= 'z') || (a >= 'A' && a <= 'Z')
}

// Topics renders this probe's topic tree under the site prefix.
func (p Probe) Topics(prefix string) entities.Topics {
	return entities.Topics{Prefix: prefix, NodeID: p.NodeID}
}

// Device is the discovery device block. Each probe is its own MQTT device rather
// than a bundle of entities on the quantum sensor's device, so that one probe
// being unplugged cannot mark the others — or the PAR sensor — unavailable: an
// MQTT CONNECT registers exactly one will, so per-probe availability requires
// per-probe device identities.
func (p Probe) Device(model, swVersion string) entities.Device {
	name := p.Name
	if name == "" {
		name = p.NodeID
	}
	if model == "" {
		model = "TEROS 12"
	}
	return entities.Device{
		Identifiers:  []string{p.NodeID},
		Name:         name,
		Manufacturer: "METER Group",
		Model:        model,
		SWVersion:    swVersion,
	}
}

func intPtr(v int) *int { return &v }

// Entities is this probe's table.
//
// PayloadPrecision is the sensor's own resolution in every row, which is finer
// than DisplayPrecision everywhere it can be. Elsewhere in this publisher the
// payload is deliberately one digit finer than the display so InfluxDB is not
// fed a pre-rounded number; here the sensor's native resolution is the floor, so
// matching it is the same guarantee — there is no further digit to keep.
func Entities() []entities.Entity {
	return []entities.Entity{
		{
			ObjectID: ObjectRawCounts,
			Name:     "Substrate raw counts",
			// Unitless on purpose. These are calibrated ADC counts, and the
			// number only becomes water content once grow-app applies the
			// equation for that zone's medium.
			Component:        "sensor",
			StateClass:       "measurement",
			Icon:             "mdi:counter",
			DisplayPrecision: intPtr(0),
			PayloadFormat:    entities.FormatFixed,
			PayloadPrecision: 2,
			Kind:             entities.SourcePolled,
			Index:            idxRawCounts,
		},
		{
			ObjectID:         ObjectTemperature,
			Name:             "Substrate temperature",
			Component:        "sensor",
			Unit:             UnitTemperature,
			StateClass:       "measurement",
			Icon:             "mdi:thermometer",
			DisplayPrecision: intPtr(1),
			PayloadFormat:    entities.FormatFixed,
			PayloadPrecision: 1,
			Kind:             entities.SourcePolled,
			Index:            idxTemperature,
		},
		{
			ObjectID:  ObjectBulkEC,
			Name:      "Substrate bulk EC",
			Component: "sensor",
			Unit:      UnitBulkEC,
			// Bulk EC is NOT the number a grower steers on — it reads roughly
			// 3–7x below the pore-water EC it is derived into, because the solids
			// and air in the measured volume do not conduct. grow-app derives and
			// labels pore-water EC separately; this row is the raw input to it.
			StateClass:       "measurement",
			Icon:             "mdi:lightning-bolt",
			DisplayPrecision: intPtr(2),
			PayloadFormat:    entities.FormatFixed,
			PayloadPrecision: 3,
			Kind:             entities.SourcePolled,
			Index:            idxBulkEC,
			// TEROS 11 omits electrical conductivity; capability is probed once
			// rather than retried, exactly as the Apogee's tilt entity is.
			Optional: true,
		},
		{
			ObjectID:      ObjectSerial,
			Name:          "Substrate probe serial",
			Component:     "sensor",
			Category:      "diagnostic",
			Icon:          "mdi:identifier",
			PayloadFormat: entities.FormatString,
			Kind:          entities.SourceStatic,
		},
	}
}

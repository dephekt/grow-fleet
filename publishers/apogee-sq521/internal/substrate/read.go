package substrate

import (
	"context"
	"fmt"
	"math"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// Sensor error codes. METER returns these IN PLACE OF a measured value, and they
// are the reason this package has a validity gate at all: they are finite signed
// floats, so every generic guard downstream accepts them. sdi12.ParseValues
// rejects only Inf and NaN, the transaction itself succeeded so nothing marks the
// reading failed, and the entity formatter renders them verbatim.
//
// What that costs, if they are allowed through: grow-app's recorder stores
// -9999 as an ordinary sample, and a history query's aggregateWindow(mean) blends
// one of them with four good ~2500-count samples into a mean near 0.2 — which any
// monotone counts-to-water-content curve maps far below oven-dry. The chart shows
// a plausible sharp dryback rather than an obvious spike, so nobody reads it as a
// fault. Publishing raw makes this worse than publishing calibrated values would,
// because grow-app has no physical range it can assert against bare counts.
//
// Detecting them is not calibration: no scaling, no unit conversion, no substrate
// term. It is the sensor telling us the number is not a measurement, in the only
// vocabulary SDI-12 gives it.
const (
	// ErrCodeCompromised means the measurement function is compromised. Per
	// METER, "the subsequent measurement values have no meaning" — so unlike the
	// other two this one poisons the whole response, not just its own field.
	ErrCodeCompromised = -9999
	// ErrCodeCalibrationLost means the sensor's stored calibrations are corrupt.
	ErrCodeCalibrationLost = -9992
	// ErrCodeLowVoltage means the supply was too low to measure.
	ErrCodeLowVoltage = -9991
)

// Plausibility bounds. The temperature and bulk-EC limits are the TEROS 11/12
// published measurement ranges. The raw-counts band has no published equivalent —
// METER specifies dielectric permittivity (1 in air to 80 in water) rather than
// the counts behind it — so it is deliberately wide, sized to catch a mis-framed
// or truncated response rather than to police a real reading. A probe in air sits
// near 1800 and saturated near 3800.
const (
	minRawCounts   = 0.0
	maxRawCounts   = 10000.0
	minTemperature = -40.0
	maxTemperature = 60.0
	minBulkECMicro = 0.0
	maxBulkECMicro = 20000.0
)

// Reading is one entity's value for one poll.
//
// A reading with OK false is not an absence of news: the caller publishes an
// empty payload to that entity's state topic, which retracts the stale retained
// value from the broker. Leaving the last good number in place would let grow-app
// keep rendering a reading from before the fault as though it were current.
type Reading struct {
	ObjectID string
	Value    float64
	OK       bool
	// Reason explains a false OK, for the log. Empty when OK.
	Reason string
}

// Read performs one aM!/aD0! transaction and maps the response onto entities.
//
// It returns a Reading for every polled entity the response covers, valid or not,
// so the caller can blank exactly the topics that went bad. A returned error
// means the transaction itself failed (no response, malformed framing, dead
// port) and nothing is known about any field — that is the caller's failure
// ladder, distinct from a sensor that answered with error codes.
func Read(ctx context.Context, m Measurer) ([]Reading, error) {
	values, err := m.Measure(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("substrate: %cM!: %w", m.Address(), err)
	}
	return readingsFrom(values)
}

// readingsFrom is the pure half of Read, split out so the mapping and the gate
// are testable without a transport.
func readingsFrom(values []float64) ([]Reading, error) {
	// A TEROS 12 returns three values and a TEROS 11 two. Fewer than two means
	// the response was not a TEROS measurement at all, which is a framing fault
	// rather than a sensor fault.
	if len(values) < 2 {
		return nil, fmt.Errorf(
			"substrate: expected at least 2 values (counts, temperature), got %d: %v", len(values), values)
	}

	// -9999 in any field invalidates every field, per METER: the measurement
	// function is compromised, so the values that follow carry no meaning. Doing
	// this before the per-field gate is what stops a plausible-looking
	// temperature riding out of a response whose counts are known to be junk.
	if compromised(values) {
		out := make([]Reading, 0, len(values))
		for _, e := range polledEntities(len(values)) {
			out = append(out, Reading{
				ObjectID: e.ObjectID,
				OK:       false,
				Reason:   "sensor reported the measurement function is compromised (-9999)",
			})
		}
		return out, nil
	}

	out := make([]Reading, 0, len(values))
	for _, e := range polledEntities(len(values)) {
		out = append(out, gate(e.ObjectID, values[e.Index]))
	}
	return out, nil
}

// polledEntities returns the polled rows this response length covers, in index
// order. A two-value response is a TEROS 11: bulk EC is absent rather than zero.
//
// Derived from the entity table rather than restating it. This used to carry its
// own object-id-to-index list, which made the Index field in Entities() dead at
// runtime and meant a fourth polled value had to be added in two places that the
// compiler would never cross-check.
func polledEntities(n int) []entities.Entity {
	var out []entities.Entity
	for _, e := range Entities() {
		// SourceStatic rows (the serial) have no index into the response at all,
		// and their zero Index would otherwise alias raw counts.
		if e.Kind != entities.SourcePolled || e.Index >= n {
			continue
		}
		out = append(out, e)
	}
	return out
}

func compromised(values []float64) bool {
	for _, v := range values {
		if isErrCode(v, ErrCodeCompromised) {
			return true
		}
	}
	return false
}

// gate turns one raw value into a Reading, rejecting sensor error codes and
// physically impossible numbers.
func gate(objectID string, v float64) Reading {
	bad := func(reason string) Reading {
		return Reading{ObjectID: objectID, OK: false, Reason: reason}
	}

	// NaN and Inf should already have been rejected by the codec, but a guard
	// whose whole job is "this is not a measurement" should not assume that.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return bad("value is not a finite number")
	}
	switch {
	case isErrCode(v, ErrCodeCalibrationLost):
		return bad("sensor reported corrupt or lost calibrations (-9992)")
	case isErrCode(v, ErrCodeLowVoltage):
		return bad("sensor reported insufficient supply voltage to measure (-9991)")
	}

	switch objectID {
	case ObjectRawCounts:
		if v < minRawCounts || v > maxRawCounts {
			return bad(fmt.Sprintf("raw counts %.2f outside the plausible band %.0f–%.0f",
				v, minRawCounts, maxRawCounts))
		}
	case ObjectTemperature:
		if v < minTemperature || v > maxTemperature {
			return bad(fmt.Sprintf("temperature %.1f °C outside the sensor's range %.0f–%.0f °C",
				v, minTemperature, maxTemperature))
		}
	case ObjectBulkEC:
		if v < minBulkECMicro || v > maxBulkECMicro {
			return bad(fmt.Sprintf("bulk EC %.0f µS/cm outside the sensor's range %.0f–%.0f µS/cm",
				v, minBulkECMicro, maxBulkECMicro))
		}
		// Native µS/cm to published mS/cm. Range-checked in the sensor's own
		// units first so the bound reads the same as the datasheet.
		v /= microsiemensPerMillisiemens
	}

	return Reading{ObjectID: objectID, Value: v, OK: true}
}

// isErrCode compares against a sentinel without relying on exact float equality
// of a parsed decimal. The codes are whole numbers far from any real reading, so
// a half-unit window cannot swallow a legitimate value.
func isErrCode(v float64, code float64) bool {
	return math.Abs(v-code) < 0.5
}

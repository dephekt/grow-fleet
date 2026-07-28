package entities

import (
	"fmt"
	"strconv"
)

// Format renders a measured value as its MQTT payload.
//
// The precision used here is PayloadPrecision, one digit finer than what the UI
// is told to display: the recorder stores the payload verbatim, so pre-rounding
// to the display precision would permanently discard resolution the sensor
// actually provided.
//
// It panics on a FormatKind it does not handle, including the unset zero value.
// That can only come from a code change — the kind is a compiled-in property of
// the table, never anything the sensor or the config can influence — so it is a
// programming error, and the alternative (falling through to fixed-point) writes
// wrong-but-plausible numbers into InfluxDB where nobody ever notices them.
func Format(e Entity, v float64) string {
	switch e.PayloadFormat {
	case FormatFixed:
		return strconv.FormatFloat(v, 'f', e.PayloadPrecision, 64)
	case FormatG6:
		// Matches the Python's f"{cal_factor:.6g}" — calibration multipliers
		// are small numbers whose magnitude varies, so significant digits are
		// the right knob rather than decimal places.
		return fmt.Sprintf("%.6g", v)
	case FormatString:
		// String entities carry pre-formatted payloads in Snapshot.Statics and
		// never reach this function. If one ever does, emit the shortest
		// representation that round-trips rather than inventing a precision.
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		panic(fmt.Sprintf("entities: entity %q has PayloadFormat %d, which Format does not handle",
			e.ObjectID, e.PayloadFormat))
	}
}

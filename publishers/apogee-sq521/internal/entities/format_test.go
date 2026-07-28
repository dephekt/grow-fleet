package entities

import "testing"

func TestFormatFixed(t *testing.T) {
	table := Build(Params{PPFDUnit: testPPFDUnit})
	ppfd := byObjectID(t, table, "ppfd")            // PayloadPrecision 1
	detector := byObjectID(t, table, "detector_mv") // PayloadPrecision 4
	tilt := byObjectID(t, table, "tilt")            // PayloadPrecision 2

	tests := []struct {
		name   string
		entity Entity
		value  float64
		want   string
	}{
		// The value from the Python's docstring example, 0+156.9289.
		{"ppfd typical", ppfd, 156.9289, "156.9"},
		{"ppfd dark", ppfd, 0, "0.0"},
		// Padded out to the payload precision even when the sensor returns an
		// integer-valued reading.
		{"ppfd integral", ppfd, 1200, "1200.0"},
		// Negative readings happen at night: the detector can read slightly
		// below zero, and the sign must survive.
		{"ppfd small negative rounds to negative zero", ppfd, -0.04, "-0.0"},
		{"ppfd negative", ppfd, -1.26, "-1.3"},
		{"ppfd rounds up", ppfd, 99.95, "100.0"},

		{"detector typical", detector, 0.0123456, "0.0123"},
		{"detector negative", detector, -0.00005, "-0.0001"},
		{"detector large", detector, 2500.123456, "2500.1235"},

		{"tilt typical", tilt, 1.234, "1.23"},
		{"tilt negative", tilt, -12.345, "-12.35"},
		{"tilt zero", tilt, 0, "0.00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Format(tc.entity, tc.value); got != tc.want {
				t.Errorf("Format(%s, %v) = %q, want %q", tc.entity.ObjectID, tc.value, got, tc.want)
			}
		})
	}
}

// cal_factor uses %.6g, matching the Python's f"{cal_factor:.6g}": calibration
// multipliers vary in magnitude, so six significant digits is the right knob.
func TestFormatG6(t *testing.T) {
	cal := byObjectID(t, Build(Params{PPFDUnit: testPPFDUnit}), "cal_factor")

	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{"unity drops the decimal point", 1, "1"},
		{"typical multiplier", 1.234567, "1.23457"},
		{"exactly six significant digits", 0.123456, "0.123456"},
		{"trailing zeros trimmed", 1.5, "1.5"},
		{"tiny switches to exponent form", 0.0000123456789, "1.23457e-05"},
		{"large switches to exponent form", 1234567, "1.23457e+06"},
		{"negative", -0.000123456789, "-0.000123457"},
		{"zero", 0, "0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Format(cal, tc.value); got != tc.want {
				t.Errorf("Format(cal_factor, %v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// String entities carry pre-formatted payloads and never reach Format; if one
// ever does, it must not silently truncate to some arbitrary precision.
func TestFormatStringFallback(t *testing.T) {
	serial := byObjectID(t, Build(Params{PPFDUnit: testPPFDUnit}), "sensor_serial")
	if got := Format(serial, 3033); got != "3033" {
		t.Errorf("Format(sensor_serial, 3033) = %q, want %q", got, "3033")
	}
	if got := Format(serial, 1.5); got != "1.5" {
		t.Errorf("Format(sensor_serial, 1.5) = %q, want %q", got, "1.5")
	}
}

// A FormatKind with no case in Format must not be absorbed by a catch-all that
// renders it as fixed-point. Adding, say, FormatPercent and forgetting the case
// would otherwise ship plausible-looking wrong payloads into InfluxDB, where the
// error is permanent and invisible.
func TestFormatRejectsUnknownKind(t *testing.T) {
	e := Entity{ObjectID: "future", PayloadFormat: FormatKind(99), PayloadPrecision: 2}
	defer func() {
		if r := recover(); r == nil {
			t.Error("Format did not panic on an unhandled FormatKind; " +
				"a new kind would silently render as fixed-point")
		}
	}()
	t.Log(Format(e, 1.2345))
}

// The same trap from the other end: a new row added to the table that forgets
// PayloadFormat entirely must fail loudly rather than inheriting fixed-point
// from the zero value.
func TestFormatRejectsUnsetKind(t *testing.T) {
	e := Entity{ObjectID: "newcomer", PayloadPrecision: 2} // PayloadFormat not set
	defer func() {
		if r := recover(); r == nil {
			t.Error("Format did not panic on an entity with no PayloadFormat; " +
				"the zero value must not be a usable format")
		}
	}()
	t.Log(Format(e, 1.2345))
}

// Zero precision must render an integer with no decimal point at all, which is
// what makes the display/payload precision split meaningful.
func TestFormatZeroPrecision(t *testing.T) {
	e := Entity{ObjectID: "synthetic", PayloadFormat: FormatFixed, PayloadPrecision: 0}
	tests := []struct {
		value float64
		want  string
	}{
		{156.9289, "157"},
		{-156.9289, "-157"},
		{0, "0"},
		{-0.4, "-0"},
	}
	for _, tc := range tests {
		if got := Format(e, tc.value); got != tc.want {
			t.Errorf("Format(precision 0, %v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

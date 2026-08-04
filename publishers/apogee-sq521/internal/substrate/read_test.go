package substrate

import (
	"math"
	"strings"
	"testing"
)

// byID indexes readings so a test can assert about one entity without depending
// on slice order.
func byID(t *testing.T, rs []Reading) map[string]Reading {
	t.Helper()
	m := make(map[string]Reading, len(rs))
	for _, r := range rs {
		if _, dup := m[r.ObjectID]; dup {
			t.Fatalf("duplicate reading for %q", r.ObjectID)
		}
		m[r.ObjectID] = r
	}
	return m
}

func TestReadingsFromTEROS12(t *testing.T) {
	// The live probe in coco, verbatim from AD0!: A+2852.70+25.8+24
	rs, err := readingsFrom([]float64{2852.70, 25.8, 24})
	if err != nil {
		t.Fatalf("readingsFrom: %v", err)
	}
	got := byID(t, rs)

	if len(got) != 3 {
		t.Fatalf("want 3 readings for a TEROS 12, got %d: %v", len(got), got)
	}
	for id, r := range got {
		if !r.OK {
			t.Errorf("%s: want OK, got not-OK (%s)", id, r.Reason)
		}
	}
	if v := got[ObjectRawCounts].Value; v != 2852.70 {
		t.Errorf("raw counts: want 2852.70 verbatim, got %v", v)
	}
	if v := got[ObjectTemperature].Value; v != 25.8 {
		t.Errorf("temperature: want 25.8 verbatim, got %v", v)
	}
	// The one deliberate transformation: native µS/cm to published mS/cm.
	if v := got[ObjectBulkEC].Value; math.Abs(v-0.024) > 1e-9 {
		t.Errorf("bulk EC: want 24 µS/cm published as 0.024 mS/cm, got %v", v)
	}
}

func TestReadingsFromTEROS11HasNoBulkEC(t *testing.T) {
	// A TEROS 11 returns two values. Bulk EC must be absent, never a fabricated
	// zero, which would read as "freshly flushed" rather than "not measured".
	rs, err := readingsFrom([]float64{2100.0, 21.5})
	if err != nil {
		t.Fatalf("readingsFrom: %v", err)
	}
	got := byID(t, rs)
	if _, present := got[ObjectBulkEC]; present {
		t.Errorf("bulk EC must be absent for a 2-value response, got %v", got[ObjectBulkEC])
	}
	if len(got) != 2 {
		t.Errorf("want 2 readings, got %d: %v", len(got), got)
	}
}

func TestReadingsFromRejectsShortResponse(t *testing.T) {
	for _, values := range [][]float64{nil, {}, {2852.7}} {
		if _, err := readingsFrom(values); err == nil {
			t.Errorf("values %v: want an error for a response too short to be a TEROS measurement", values)
		}
	}
}

// The blocker this package exists to prevent. -9999 et al are finite floats, so
// nothing upstream rejects them; if they reach InfluxDB a windowed mean blends
// one into the surrounding good samples and renders as a plausible dryback.
func TestSentinelsAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   map[string]bool // objectID -> OK
		reason string
	}{
		{
			name:   "low voltage in bulk EC only invalidates bulk EC",
			values: []float64{2852.70, 25.8, ErrCodeLowVoltage},
			want: map[string]bool{
				ObjectRawCounts:   true,
				ObjectTemperature: true,
				ObjectBulkEC:      false,
			},
			reason: "voltage",
		},
		{
			name:   "lost calibration in counts only invalidates counts",
			values: []float64{ErrCodeCalibrationLost, 25.8, 24},
			want: map[string]bool{
				ObjectRawCounts:   false,
				ObjectTemperature: true,
				ObjectBulkEC:      true,
			},
			reason: "calibration",
		},
		{
			// METER: with -9999 "the subsequent measurement values have no
			// meaning", so a plausible-looking temperature must not survive a
			// response whose counts are known junk.
			name:   "compromised poisons every field, not just its own",
			values: []float64{ErrCodeCompromised, 25.8, 24},
			want: map[string]bool{
				ObjectRawCounts:   false,
				ObjectTemperature: false,
				ObjectBulkEC:      false,
			},
			reason: "compromised",
		},
		{
			name:   "compromised in a trailing field still poisons the leading ones",
			values: []float64{2852.70, 25.8, ErrCodeCompromised},
			want: map[string]bool{
				ObjectRawCounts:   false,
				ObjectTemperature: false,
				ObjectBulkEC:      false,
			},
			reason: "compromised",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := readingsFrom(tc.values)
			if err != nil {
				t.Fatalf("readingsFrom: %v", err)
			}
			got := byID(t, rs)
			for id, wantOK := range tc.want {
				r, present := got[id]
				if !present {
					t.Fatalf("%s: missing from readings", id)
				}
				if r.OK != wantOK {
					t.Errorf("%s: OK = %v, want %v (reason %q)", id, r.OK, wantOK, r.Reason)
				}
				if !wantOK {
					if r.Reason == "" {
						t.Errorf("%s: rejected with no reason", id)
					}
					if r.Value != 0 {
						t.Errorf("%s: rejected reading must not carry a value, got %v", id, r.Value)
					}
					if !strings.Contains(strings.ToLower(r.Reason), tc.reason) {
						t.Errorf("%s: reason %q does not mention %q", id, r.Reason, tc.reason)
					}
				}
			}
		})
	}
}

func TestOutOfRangeValuesAreRejected(t *testing.T) {
	tests := []struct {
		name     string
		values   []float64
		objectID string
	}{
		{"temperature below the sensor's range", []float64{2852.7, -41, 24}, ObjectTemperature},
		{"temperature above the sensor's range", []float64{2852.7, 61, 24}, ObjectTemperature},
		{"bulk EC above the sensor's range", []float64{2852.7, 25.8, 20001}, ObjectBulkEC},
		{"bulk EC negative", []float64{2852.7, 25.8, -1}, ObjectBulkEC},
		{"raw counts implausibly high", []float64{10001, 25.8, 24}, ObjectRawCounts},
		{"raw counts negative", []float64{-1, 25.8, 24}, ObjectRawCounts},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := readingsFrom(tc.values)
			if err != nil {
				t.Fatalf("readingsFrom: %v", err)
			}
			r := byID(t, rs)[tc.objectID]
			if r.OK {
				t.Errorf("%s: want rejected, got value %v", tc.objectID, r.Value)
			}
		})
	}
}

func TestRangeBoundariesAreInclusive(t *testing.T) {
	// A probe genuinely sitting at 0 °C or reading 0 bulk EC in RO-flushed coco
	// must not be discarded by the guard meant to catch garbage.
	rs, err := readingsFrom([]float64{0, -40, 0})
	if err != nil {
		t.Fatalf("readingsFrom: %v", err)
	}
	for id, r := range byID(t, rs) {
		if !r.OK {
			t.Errorf("%s: boundary value rejected (%s)", id, r.Reason)
		}
	}
}

func TestNonFiniteIsRejected(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		r := gate(ObjectTemperature, v)
		if r.OK {
			t.Errorf("value %v: want rejected", v)
		}
	}
}

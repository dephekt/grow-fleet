package sdi12

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseMeasureHeader(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantReady time.Duration
		wantCount int
		wantErr   error
	}{
		// The SQ-521 answers every M/V command with a one-second conversion.
		{name: "0M! ppfd", line: "00011", wantReady: time.Second, wantCount: 1},
		{name: "0M1! detector mV", line: "00011", wantReady: time.Second, wantCount: 1},
		{name: "0M4! tilt", line: "00011", wantReady: time.Second, wantCount: 1},
		{name: "0V! coefficients", line: "00013", wantReady: time.Second, wantCount: 3},
		{name: "data ready immediately", line: "00001", wantCount: 1},
		{name: "long conversion", line: "01202", wantReady: 120 * time.Second, wantCount: 2},
		{name: "two-digit count", line: "000012", wantCount: 12},
		{name: "unsupported command", line: "00000", wantCount: 0},
		{name: "trailing whitespace tolerated", line: "00011 ", wantReady: time.Second, wantCount: 1},
		{name: "non-zero address", line: "30011", wantReady: time.Second, wantCount: 1},

		// SDI-12 defines n as one digit for aM!/aV! and two for aC!, so 99 is the
		// widest count the protocol can express. An unbounded count is not a
		// parsing curiosity: it flows straight into make() in Client.collect.
		{name: "largest accepted count", line: "000199", wantReady: time.Second, wantCount: 99},
		{name: "smallest rejected count", line: "0001100", wantErr: ErrProtocol},
		// The three crash inputs. Nine digits reserves 8 GB (an OOM-kill on a
		// Pi), ten produces a *fatal* runtime out-of-memory that no recover() can
		// catch, and twenty overflows int to a negative capacity and panics. All
		// three are reachable from a 14-byte line of adapter noise.
		{name: "nine-digit count reserves gigabytes", line: "0001" + strings.Repeat("9", 9), wantErr: ErrProtocol},
		{name: "ten-digit count is a fatal oom", line: "0001" + strings.Repeat("9", 10), wantErr: ErrProtocol},
		{name: "twenty-digit count overflows int", line: "0001" + strings.Repeat("9", 20), wantErr: ErrProtocol},

		{name: "empty is silence", line: "", wantErr: ErrNoResponse},
		{name: "blank is silence", line: "   ", wantErr: ErrNoResponse},
		{name: "too short", line: "0001", wantErr: ErrProtocol},
		{name: "ttt not digits", line: "0abc1", wantErr: ErrProtocol},
		{name: "count not digits", line: "0001x", wantErr: ErrProtocol},
		{name: "signed count", line: "0001+1", wantErr: ErrProtocol},
		{name: "data line mistaken for header", line: "0+156.9289", wantErr: ErrProtocol},
		{name: "identity mistaken for header", line: "013APOGEE  SQ-5211043043", wantErr: ErrProtocol},
		{name: "pure garbage", line: "\x00\xff??", wantErr: ErrProtocol},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready, count, err := ParseMeasureHeader(tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseMeasureHeader(%q) error = %v, want %v", tc.line, err, tc.wantErr)
				}
				// The Python fabricated (1s, 1) here and then sent D0! on the
				// strength of the guess. Nothing may be inferred from garbage.
				if ready != 0 || count != 0 {
					t.Fatalf("ParseMeasureHeader(%q) fabricated (%v, %d) alongside its error; want zero values",
						tc.line, ready, count)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMeasureHeader(%q) unexpected error: %v", tc.line, err)
			}
			if ready != tc.wantReady || count != tc.wantCount {
				t.Fatalf("ParseMeasureHeader(%q) = (%v, %d), want (%v, %d)",
					tc.line, ready, count, tc.wantReady, tc.wantCount)
			}
		})
	}
}

func TestParseValues(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    []float64
		wantErr error
	}{
		{name: "0D0! ppfd", line: "0+156.9289", want: []float64{156.9289}},
		{name: "0D0! detector mV", line: "0+15.9124", want: []float64{15.9124}},
		{name: "0D0! tilt", line: "0+1.2", want: []float64{1.2}},
		// The SQ-521's detector reads slightly below zero in true darkness; that
		// is a real measurement, not corruption.
		{name: "negative in darkness", line: "0-0.1147", want: []float64{-0.1147}},
		{name: "negative ppfd", line: "0-0.4", want: []float64{-0.4}},
		// Every value must survive. The Python kept findall(...)[0] and threw
		// the rest away, which is why 0M!'s true value count was never known.
		{name: "0V! coefficients", line: "0+0.05387+0.0+0.0", want: []float64{0.05387, 0, 0}},
		{name: "mixed signs", line: "0+1.1-2.2+3.3", want: []float64{1.1, -2.2, 3.3}},
		{name: "integers", line: "0+157", want: []float64{157}},
		{name: "many values", line: "0+1+2+3+4+5", want: []float64{1, 2, 3, 4, 5}},
		{name: "no address echo", line: "+42.5", want: []float64{42.5}},
		{name: "letter address", line: "a+42.5", want: []float64{42.5}},
		{name: "surrounding whitespace", line: "  0+156.9289  ", want: []float64{156.9289}},

		{name: "empty is silence", line: "", wantErr: ErrNoResponse},
		// A bare address is the sensor saying "nothing further" — well formed,
		// so the D-continuation loop stops instead of reporting corruption.
		{name: "bare address", line: "0", wantErr: ErrNoValues},
		{name: "unsigned value", line: "0156.9289", wantErr: ErrProtocol},
		{name: "trailing garbage", line: "0+156.9289xyz", wantErr: ErrProtocol},
		{name: "sign with no digits", line: "0+", wantErr: ErrProtocol},
		{name: "two decimal points", line: "0+1.2.3", wantErr: ErrProtocol},
		{name: "infinity rejected", line: "0+Inf", wantErr: ErrProtocol},
		{name: "nan rejected", line: "0+NaN", wantErr: ErrProtocol},
		{name: "hex float rejected", line: "0+0x1p3", wantErr: ErrProtocol},
		{name: "exponent rejected", line: "0+1.2e5", wantErr: ErrProtocol},
		{name: "header mistaken for data", line: "00011", wantErr: ErrProtocol},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseValues(tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseValues(%q) error = %v, want %v", tc.line, err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("ParseValues(%q) returned %v alongside its error", tc.line, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseValues(%q) unexpected error: %v", tc.line, err)
			}
			assertFloats(t, got, tc.want)
		})
	}
}

func TestParseIdentity(t *testing.T) {
	// Fixed-width SDI-12 v1.3 layout as the SQ-521 emits it:
	// addr "0" | version "13" | vendor "APOGEE  " | model "SQ-521" | fw "104" | serial "3043".
	const sq521 = "013APOGEE  SQ-5211043043"

	tests := []struct {
		name    string
		line    string
		want    Identity
		wantErr error
	}{
		{
			name: "0I! sq-521",
			line: sq521,
			want: Identity{
				Address: '0', Version: "13", Vendor: "APOGEE",
				Model: "SQ-521", Firmware: "104", Serial: "3043",
			},
		},
		{
			name: "padded model and empty serial",
			line: "013APOGEE  SQ-500100",
			want: Identity{
				Address: '0', Version: "13", Vendor: "APOGEE",
				Model: "SQ-500", Firmware: "100", Serial: "",
			},
		},
		{
			name: "13-byte serial",
			line: "113APOGEE  SQ-5211041234567890123",
			want: Identity{
				Address: '1', Version: "13", Vendor: "APOGEE",
				Model: "SQ-521", Firmware: "104", Serial: "1234567890123",
			},
		},
		{name: "empty is silence", line: "", wantErr: ErrNoResponse},
		{name: "blank is silence", line: "   ", wantErr: ErrNoResponse},
		{name: "one byte short", line: "013APOGEE  SQ-52110", wantErr: ErrProtocol},
		{name: "service request", line: "0", wantErr: ErrProtocol},
		{name: "data line", line: "0+156.9289", wantErr: ErrProtocol},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIdentity(tc.line)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseIdentity(%q) error = %v, want %v", tc.line, err, tc.wantErr)
				}
				if got != (Identity{}) {
					t.Fatalf("ParseIdentity(%q) returned %+v alongside its error", tc.line, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIdentity(%q) unexpected error: %v", tc.line, err)
			}
			if got != tc.want {
				t.Fatalf("ParseIdentity(%q) = %+v, want %+v", tc.line, got, tc.want)
			}
		})
	}
}

func TestIsServiceRequest(t *testing.T) {
	tests := []struct {
		line string
		addr byte
		want bool
	}{
		{line: "0", addr: '0', want: true},
		{line: "0\r", addr: '0', want: true}, // stray CR the framer did not strip
		{line: " 0 ", addr: '0', want: true},
		{line: "3", addr: '3', want: true},
		{line: "", addr: '0', want: false},
		{line: "1", addr: '0', want: false},
		{line: "00", addr: '0', want: false},
		{line: "0+156.9289", addr: '0', want: false},
		{line: "00011", addr: '0', want: false},
	}
	for _, tc := range tests {
		if got := IsServiceRequest(tc.line, tc.addr); got != tc.want {
			t.Errorf("IsServiceRequest(%q, %q) = %v, want %v", tc.line, string(tc.addr), got, tc.want)
		}
	}
}

func assertFloats(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d values %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		// Values are parsed, never computed, so they must round-trip exactly;
		// a tolerance would hide a mis-split field.
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("value %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The parsers must never panic on arbitrary bytes: everything on this wire
// arrives from a USB device that may be mid-reset.
func TestParsersToleratePartialLines(t *testing.T) {
	const full = "013APOGEE  SQ-5211043043"
	for i := 0; i <= len(full); i++ {
		prefix := full[:i]
		_, _, _ = ParseMeasureHeader(prefix)
		_, _ = ParseValues(prefix)
		_, _ = ParseIdentity(prefix)
	}
	for _, s := range []string{"+", "-", "0+", "0-", "\r", "\x00", strings.Repeat("+", 64)} {
		_, _, _ = ParseMeasureHeader(s)
		_, _ = ParseValues(s)
		_, _ = ParseIdentity(s)
	}
}

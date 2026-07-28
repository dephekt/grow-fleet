package timezone

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSyntheticTZifRoundTrip is the proof the whole package rests on: a TZif v2
// blob whose only real content is a POSIX footer round-trips through
// time.LoadLocationFromTZData into correct DST behaviour. If this fails,
// nothing else in the package can work and the approach has to be rethought
// rather than patched.
func TestSyntheticTZifRoundTrip(t *testing.T) {
	loc, err := time.LoadLocationFromTZData("probe", tzif("GMT0BST,M3.5.0/1,M10.5.0", 0, "AAA"))
	if err != nil {
		t.Fatalf("LoadLocationFromTZData rejected the synthetic blob: %v", err)
	}
	// Europe/London's 2026 spring transition, to the second.
	if name, off := time.Date(2026, 3, 29, 0, 59, 59, 0, time.UTC).In(loc).Zone(); name != "GMT" || off != 0 {
		t.Errorf("one second before the transition = %s/%d, want GMT/0", name, off)
	}
	if name, off := time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC).In(loc).Zone(); name != "BST" || off != 3600 {
		t.Errorf("at the transition = %s/%d, want BST/3600", name, off)
	}
	if !time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(loc).IsDST() {
		t.Error("July is not reported as DST")
	}
}

// TestTZifLayout pins the byte offsets documented on tzif. The reader locates
// the v2 header by computing the v1 block's length from the counts, so a change
// to either the counts or the block sizes silently shifts everything after it.
func TestTZifLayout(t *testing.T) {
	const posix = "GMT0BST,M3.5.0/1,M10.5.0"
	b := tzif(posix, 0, "AAA")

	if got, want := len(b), 108+len(posix)+2; got != want {
		t.Fatalf("blob length = %d, want %d", got, want)
	}
	if string(b[0:4]) != "TZif" {
		t.Errorf("magic = %q, want %q", b[0:4], "TZif")
	}
	if b[4] != '2' {
		t.Errorf("version = %q, want '2'", b[4])
	}
	if !bytes.Equal(b[5:20], make([]byte, 15)) {
		t.Errorf("version padding = %x, want 15 NUL", b[5:20])
	}
	// isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt.
	wantCounts := [6]uint32{0, 0, 0, 0, 1, 4}
	for i, want := range wantCounts {
		if got := binary.BigEndian.Uint32(b[20+4*i:]); got != want {
			t.Errorf("count[%d] = %d, want %d", i, got, want)
		}
	}
	// The v2 header must repeat the v1 header byte for byte, at offset 54,
	// which is where the reader's skip arithmetic lands.
	if !bytes.Equal(b[0:44], b[54:98]) {
		t.Error("v2 header at offset 54 does not repeat the v1 header")
	}
	if !bytes.Equal(b[44:54], b[98:108]) {
		t.Error("v2 data at offset 98 does not repeat the v1 data")
	}
	if got := b[108]; got != '\n' {
		t.Errorf("footer does not start with a newline at offset 108, got %q", got)
	}
	if got := string(b[109 : len(b)-1]); got != posix {
		t.Errorf("footer payload = %q, want %q", got, posix)
	}
	if got := b[len(b)-1]; got != '\n' {
		t.Errorf("footer does not end with a newline, got %q", got)
	}
}

// footerOf extracts the POSIX footer from a real tzdata file: everything after
// the last newline, ignoring the trailing one. Skips when tzdata is absent or
// the file predates TZif v2, since neither is something this package can fix.
func footerOf(t *testing.T, zone string) string {
	t.Helper()
	path := filepath.Join("/usr/share/zoneinfo", zone)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("tzdata unavailable (%s): %v", path, err)
	}
	if len(b) < 2 || b[len(b)-1] != '\n' {
		t.Skipf("%s has no TZif v2+ footer", path)
	}
	rest := b[:len(b)-1]
	i := bytes.LastIndexByte(rest, '\n')
	if i < 0 || i+1 >= len(rest) {
		t.Skipf("%s has an empty footer", path)
	}
	return string(rest[i+1:])
}

// TestParseRealFooters runs genuine footers from the system tzdata and asserts
// exact zone names and offsets one second either side of the 2026 transitions.
//
// Each case is also cross-checked against the same zone loaded normally, which
// holds no matter which tzdata version is installed and proves the synthetic
// blob reproduces the real zone rather than merely being self-consistent. The
// hardcoded offsets are what stop that cross-check from degrading into "two
// implementations agree with each other"; they are skipped, loudly, if the
// installed footer no longer matches the one they were written against.
func TestParseRealFooters(t *testing.T) {
	type instant struct {
		utc    time.Time
		name   string
		offset int
	}
	tests := []struct {
		zone   string
		footer string
		want   []instant
	}{
		{
			zone:   "Europe/London",
			footer: "GMT0BST,M3.5.0/1,M10.5.0",
			want: []instant{
				{time.Date(2026, 3, 29, 0, 59, 59, 0, time.UTC), "GMT", 0},
				{time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), "BST", 3600},
				{time.Date(2026, 10, 25, 0, 59, 59, 0, time.UTC), "BST", 3600},
				{time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC), "GMT", 0},
			},
		},
		{
			zone:   "America/Chicago",
			footer: "CST6CDT,M3.2.0,M11.1.0",
			want: []instant{
				{time.Date(2026, 3, 8, 7, 59, 59, 0, time.UTC), "CST", -21600},
				{time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC), "CDT", -18000},
				{time.Date(2026, 11, 1, 6, 59, 59, 0, time.UTC), "CDT", -18000},
				{time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), "CST", -21600},
			},
		},
		{
			// Southern hemisphere: the DST window straddles the year
			// boundary, so January is daylight time and July is standard.
			zone:   "Australia/Sydney",
			footer: "AEST-10AEDT,M10.1.0,M4.1.0/3",
			want: []instant{
				{time.Date(2026, 4, 4, 15, 59, 59, 0, time.UTC), "AEDT", 39600},
				{time.Date(2026, 4, 4, 16, 0, 0, 0, time.UTC), "AEST", 36000},
				{time.Date(2026, 10, 3, 15, 59, 59, 0, time.UTC), "AEST", 36000},
				{time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC), "AEDT", 39600},
			},
		},
		{
			// A fixed zone: no comma, so the DST-dropped guard must not fire.
			zone:   "UTC",
			footer: "UTC0",
			want: []instant{
				{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), "UTC", 0},
				{time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC), "UTC", 0},
			},
		},
		{
			// A fixed zone with a half-hour offset, which exercises
			// tzsetOffset's minutes handling.
			zone:   "Asia/Kolkata",
			footer: "IST-5:30",
			want: []instant{
				{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), "IST", 19800},
				{time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC), "IST", 19800},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.zone, func(t *testing.T) {
			footer := footerOf(t, tt.zone)

			loc, err := Parse(footer)
			if err != nil {
				t.Fatalf("Parse(%q) from %s: %v", footer, tt.zone, err)
			}
			if loc.String() != footer {
				t.Errorf("Location name = %q, want the POSIX string %q", loc.String(), footer)
			}

			real, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Skipf("cannot load %s for cross-check: %v", tt.zone, err)
			}

			if footer != tt.footer {
				t.Logf("installed footer for %s is %q, not the recorded %q; "+
					"checking against the real zone only", tt.zone, footer, tt.footer)
			}
			for _, w := range tt.want {
				gotName, gotOff := w.utc.In(loc).Zone()
				wantName, wantOff := w.utc.In(real).Zone()
				if gotName != wantName || gotOff != wantOff {
					t.Errorf("at %s: synthetic blob gives %s/%d, real %s gives %s/%d",
						w.utc.Format(time.RFC3339), gotName, gotOff, tt.zone, wantName, wantOff)
				}
				if footer != tt.footer {
					continue
				}
				if gotName != w.name || gotOff != w.offset {
					t.Errorf("at %s: got %s/%d, want %s/%d",
						w.utc.Format(time.RFC3339), gotName, gotOff, w.name, w.offset)
				}
			}
		})
	}
}

// TestLoadTimeCacheDoesNotShadowTheExtendString pins the claim the package
// comment makes about LoadLocationFromTZData's load-time cache
// (zoneinfo.go:161-168): it is pre-filled at load and consulted before the
// binary search, but it is narrowed to the window tzset reports, so instants
// outside it still reach the extend string.
//
// Nothing else in this file can catch that being wrong. Every other test either
// builds a fresh Location per instant or stays inside one DST window, and a
// cache that wrongly covered all time would answer every one of them correctly
// with the zone that happened to be in force at load. This test reuses a single
// Location and walks it across sixteen years in an order chosen to be hostile
// to a stale window — far future before far past, standard and daylight
// alternating — so a cache that failed to narrow would return one zone for
// instants that need both.
func TestLoadTimeCacheDoesNotShadowTheExtendString(t *testing.T) {
	const zone = "Europe/London"
	footer := footerOf(t, zone)

	loc, err := Parse(footer)
	if err != nil {
		t.Fatalf("Parse(%q) from %s: %v", footer, zone, err)
	}
	real, err := time.LoadLocation(zone)
	if err != nil {
		t.Skipf("cannot load %s for cross-check: %v", zone, err)
	}

	// Deliberately out of chronological order, and each pair straddles a
	// transition to the second.
	instants := []time.Time{
		time.Date(2035, 7, 15, 12, 0, 0, 0, time.UTC),   // far future, daylight
		time.Date(2019, 1, 15, 12, 0, 0, 0, time.UTC),   // far past, standard
		time.Date(2035, 3, 25, 0, 59, 59, 0, time.UTC),  // future, just before spring
		time.Date(2035, 3, 25, 1, 0, 0, 0, time.UTC),    // future, at spring
		time.Date(2019, 10, 27, 0, 59, 59, 0, time.UTC), // past, just before autumn
		time.Date(2019, 10, 27, 1, 0, 0, 0, time.UTC),   // past, at autumn
		time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),   // back to the middle
		time.Date(2019, 7, 15, 12, 0, 0, 0, time.UTC),   // past, daylight
		time.Date(2035, 1, 15, 12, 0, 0, 0, time.UTC),   // future, standard
	}

	var sawStandard, sawDaylight bool
	for _, ts := range instants {
		gotName, gotOff := ts.In(loc).Zone()
		wantName, wantOff := ts.In(real).Zone()
		if gotName != wantName || gotOff != wantOff {
			t.Errorf("at %s: synthetic blob gives %s/%d, real %s gives %s/%d",
				ts.Format(time.RFC3339), gotName, gotOff, zone, wantName, wantOff)
		}
		switch gotOff {
		case 0:
			sawStandard = true
		case 3600:
			sawDaylight = true
		}
	}

	// If the cache had swallowed the whole timeline, every instant above would
	// have reported the same offset and the cross-check could still have passed
	// against an equally-broken expectation. It cannot pass having seen both.
	if !sawStandard || !sawDaylight {
		t.Errorf("saw standard=%v daylight=%v across %d instants; expected both, "+
			"otherwise this test is not exercising the extend string at all",
			sawStandard, sawDaylight, len(instants))
	}
}

func TestParseByteGuards(t *testing.T) {
	const valid = "GMT0BST,M3.5.0/1,M10.5.0"

	tests := []struct {
		name  string
		posix string
		want  error
	}{
		{"empty", "", ErrEmpty},
		{"NUL in the middle", "GMT0\x00BST,M3.5.0/1,M10.5.0", ErrNUL},
		{"trailing NUL", valid + "\x00", ErrNUL},
		{"trailing newline", valid + "\n", ErrControlByte},
		{"newline inside an abbreviation", "GMT0\nBST", ErrControlByte},
		{"trailing carriage return", valid + "\r", ErrControlByte},
		{"tab", "GMT0\tBST", ErrControlByte},
		{"DEL", "GMT0BST\x7f", ErrControlByte},
		{"non-ASCII", "GMT0BST\xc3\xa9", ErrControlByte},
		// Offset 0 needs its own rows. Every case above puts the bad byte at
		// offset 4 or later, so the loop's start bound is unpinned by them:
		// starting it at 1 instead of 0 passes all of them while accepting
		// "\r"+valid, whose zone is then literally named "\rGMT". A leading
		// CR is the realistic corruption — a CRLF-edited state file — and it
		// is the quietest, since the offsets and DST rules still come out
		// right and only the name carries the damage. See
		// TestControlByteGuardPinsOffsetZero for that demonstration.
		{"leading NUL", "\x00" + valid, ErrNUL},
		{"leading newline", "\n" + valid, ErrControlByte},
		{"leading carriage return", "\r" + valid, ErrControlByte},
	}
	// The length boundary itself is covered precisely by
	// TestParseLengthBoundary rather than by an eyeballed literal here.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse(tt.posix); !errors.Is(err, tt.want) {
				t.Errorf("Parse(%q) error = %v, want %v", tt.posix, err, tt.want)
			}
		})
	}
}

// TestControlByteGuardIsLoadBearing demonstrates what the control-byte guard
// actually prevents, so that removing it as "defensive noise" fails here with
// an explanation rather than quietly passing.
//
// "GMT0BST\n" is not rejected by any of the three guards on its own: tzsetName
// happily takes "BST\n" as an abbreviation, tzset then substitutes tzcode's
// implicit US rules, and the result contains no comma so the DST-dropped guard
// stays silent. The zone that comes out is named with an embedded newline and
// switches on American dates.
func TestControlByteGuardIsLoadBearing(t *testing.T) {
	const corrupt = "GMT0BST\n" // a UK footer as `echo` would leave it

	raw, err := time.LoadLocationFromTZData("raw", tzif(corrupt, 0, "AAA"))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	name, _ := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(raw).Zone()
	if name == "AAA" {
		t.Fatal("setup: the standard library rejected this after all, so the " +
			"control-byte guard is no longer what prevents it")
	}
	if !strings.Contains(name, "\n") {
		t.Errorf("setup: expected a zone name containing a newline, got %q", name)
	}
	// American rules, not British: DST would still be running in late October.
	if !time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC).In(raw).IsDST() {
		t.Error("setup: expected the implicit US rules to keep DST active past " +
			"the UK transition; the hazard this guard covers has changed")
	}

	if _, err := Parse(corrupt); !errors.Is(err, ErrControlByte) {
		t.Errorf("Parse(%q) error = %v, want ErrControlByte", corrupt, err)
	}
}

// TestControlByteGuardPinsOffsetZero is the offset-0 companion to
// TestControlByteGuardIsLoadBearing, and it exists because the byte loop's
// start bound is otherwise untested: every other control-byte case in this file
// places the bad byte at offset 4 or later, so starting the loop at 1 rather
// than 0 passes all of them.
//
// The input is a valid UK footer with a leading carriage return — what a
// CRLF-edited state file leaves behind. It is a quieter corruption than the
// trailing-newline case above, which is why it deserves its own demonstration:
// there, the string picked up tzcode's implicit US rules and switched on the
// wrong dates. Here the abbreviation "\rGMT" is followed by a well-formed
// offset and rule pair, so tzset consumes the whole string, the transitions
// land on the correct instants to the second, and observesDST reports true.
// Guards 2 and 3 therefore both stay silent, and the only damage is a zone name
// with an invisible byte at the front of it — one that this daemon would then
// echo to the retained state topic and print into every log line.
func TestControlByteGuardPinsOffsetZero(t *testing.T) {
	const corrupt = "\rGMT0BST,M3.5.0/1,M10.5.0" // a UK footer read from a CRLF file

	raw, err := time.LoadLocationFromTZData("raw", tzif(corrupt, 0, "AAA"))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	name, off := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(raw).Zone()
	if name == "AAA" {
		t.Fatal("setup: the standard library rejected this after all, so the " +
			"control-byte guard is no longer what prevents it")
	}
	if name != "\rGMT" || off != 0 {
		t.Errorf("setup: standard zone = %q/%d, want \"\\rGMT\"/0; the corruption "+
			"this case demonstrates has changed shape", name, off)
	}

	// The rules survive intact, which is what makes this the quiet case: the
	// other two guards have nothing to catch.
	if name, off := time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC).In(raw).Zone(); name != "BST" || off != 3600 {
		t.Errorf("setup: at the spring transition = %s/%d, want BST/3600; the DST "+
			"rules were expected to parse correctly here", name, off)
	}
	if !observesDST(raw, 2026) {
		t.Error("setup: expected DST to be observed, which is what keeps the " +
			"DST-dropped guard from firing and leaves guard 1 solely responsible")
	}

	if _, err := Parse(corrupt); !errors.Is(err, ErrControlByte) {
		t.Errorf("Parse(%q) error = %v, want ErrControlByte", corrupt, err)
	}
}

// TestParseAcceptsSpaceInAbbreviation documents an accepted limit rather than a
// desired behaviour. The control-byte guard deliberately stops at invisible
// bytes, leaving printable ones to tzset, and tzsetName treats a space as an
// ordinary abbreviation character. So " GMT0" is accepted as a fixed zone
// named " GMT".
//
// This is tolerable where a newline is not: the value is echoed back verbatim
// and printed as it was received, so nothing is hidden from the operator, and
// with no DST rules involved the offset is simply correct. No tzdata footer
// contains a space, so the reconciler cannot produce this.
func TestParseAcceptsSpaceInAbbreviation(t *testing.T) {
	loc, err := Parse(" GMT0")
	if err != nil {
		t.Fatalf("Parse(\" GMT0\"): %v", err)
	}
	if name, off := time.Now().In(loc).Zone(); name != " GMT" || off != 0 {
		t.Errorf("got %q/%d, want \" GMT\"/0", name, off)
	}
}

// TestMaxLenMatchesTheContract pins the constant to its literal value.
//
// TestParseLengthBoundary derives its inputs from MaxLen, so it verifies the
// guard is applied consistently but cannot notice the constant itself moving.
// The number is not ours to choose: grow-app documents a 63-byte ceiling and
// the ESPHome component's TZ_BUFFER_SIZE is 64 including the NUL
// (timezone_text.cpp:102). Raising it would accept values a fleet node rejects,
// which is precisely the divergence the reconciler cannot see.
func TestMaxLenMatchesTheContract(t *testing.T) {
	if MaxLen != 63 {
		t.Errorf("MaxLen = %d, want 63 to match grow-app and the ESPHome component", MaxLen)
	}
}

// TestParseLengthBoundary pins the guard at exactly MaxLen so an off-by-one in
// either direction is caught: a string of exactly MaxLen bytes must be
// accepted, and the same string plus one byte must not be.
func TestParseLengthBoundary(t *testing.T) {
	// A valid POSIX string padded to exactly MaxLen with extra abbreviation
	// letters, which tzsetName accepts.
	posix := "GMT0BST,M3.5.0/1,M10.5.0"
	for len(posix) < MaxLen {
		posix = "A" + posix
	}
	if len(posix) != MaxLen {
		t.Fatalf("test setup: length %d, want %d", len(posix), MaxLen)
	}
	if _, err := Parse(posix); err != nil {
		t.Errorf("Parse of a %d-byte string failed: %v", MaxLen, err)
	}
	if _, err := Parse(posix + "A"); !errors.Is(err, ErrTooLong) {
		t.Errorf("Parse of a %d-byte string error = %v, want ErrTooLong", MaxLen+1, err)
	}
}

// TestParseRejectsUnparsable covers guard 2 in isolation. Every input here is
// comma-free, so the DST-dropped guard cannot fire and only the differential
// probe can be responsible for the rejection.
func TestParseRejectsUnparsable(t *testing.T) {
	tests := []struct {
		name  string
		posix string
	}{
		{"not remotely a tz string", "hello world"},
		{"punctuation", "!!!"},
		{"digits only", "12345"},
		{"abbreviation with no offset", "GMT"},
		{"offset with no abbreviation", "5"},
		{"abbreviation too short", "GM5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := Parse(tt.posix)
			if err == nil {
				name, off := time.Now().In(loc).Zone()
				t.Fatalf("Parse(%q) succeeded, giving %s/%d", tt.posix, name, off)
			}
			if !errors.Is(err, ErrUnparsable) {
				t.Errorf("Parse(%q) error = %v, want ErrUnparsable", tt.posix, err)
			}
		})
	}
}

// TestParseRejectsDroppedDST covers guard 3: strings the standard library
// accepts with ok=true after silently throwing the DST rules away
// (zoneinfo.go:295-298). Without this guard the daemon would run an hour off
// for half the year while reporting success.
func TestParseRejectsDroppedDST(t *testing.T) {
	tests := []struct {
		name  string
		posix string
	}{
		// The headline case: DST abbreviation missing, so tzset takes the
		// "no daylight savings time" branch and discards both rules.
		{"missing DST abbreviation", "GMT0,M3.5.0/1,M10.5.0"},
		{"missing DST abbreviation, US rules", "CST6,M3.2.0,M11.1.0"},
		{"fixed zone with a stray rule", "UTC0,M3.5.0/1,M10.5.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Establish that the standard library really does accept these,
			// so this test is guarding against library leniency rather than
			// asserting something trivially true.
			raw, err := time.LoadLocationFromTZData("raw", tzif(tt.posix, 0, "AAA"))
			if err != nil {
				t.Fatalf("setup: LoadLocationFromTZData(%q): %v", tt.posix, err)
			}
			if name, _ := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(raw).Zone(); name == "AAA" {
				t.Fatalf("setup: %q did not in fact parse, so this case does not "+
					"exercise the DST-dropped guard", tt.posix)
			}

			if _, err := Parse(tt.posix); !errors.Is(err, ErrDSTDropped) {
				t.Errorf("Parse(%q) error = %v, want ErrDSTDropped", tt.posix, err)
			}
		})
	}
}

// TestParseESPHomeTypo pins the documented divergence from the fleet: the typo
// in the ESPHome component's own comment, a space where a comma belongs. Go
// requires the rule pair to consume the whole string (zoneinfo.go:330), so this
// is rejected outright rather than reaching the DST-dropped guard.
func TestParseESPHomeTypo(t *testing.T) {
	const posix = "CST6CDT M3.2.0,M11.1.0/2"
	_, err := Parse(posix)
	if !errors.Is(err, ErrUnparsable) {
		t.Errorf("Parse(%q) error = %v, want ErrUnparsable", posix, err)
	}
}

// TestParseAcceptsDSTWithoutRules covers the case adjacent to guard 3 that must
// NOT be rejected: a DST abbreviation with no explicit rules, where tzset
// substitutes the US defaults (",M3.2.0,M11.1.0"). There is no comma in the
// input, so the guard stays out of the way, and the result genuinely observes
// DST.
func TestParseAcceptsDSTWithoutRules(t *testing.T) {
	loc, err := Parse("EST5EDT")
	if err != nil {
		t.Fatalf("Parse(EST5EDT): %v", err)
	}
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone(); name != "EDT" || off != -14400 {
		t.Errorf("July = %s/%d, want EDT/-14400", name, off)
	}
	if name, off := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone(); name != "EST" || off != -18000 {
		t.Errorf("January = %s/%d, want EST/-18000", name, off)
	}
}

// TestObservesDST exercises the guard-3 helper directly, including the
// southern-hemisphere case its 366-day sweep exists to handle without a special
// case, and the fixed zones where it must report false.
func TestObservesDST(t *testing.T) {
	tests := []struct {
		posix string
		want  bool
	}{
		{"GMT0BST,M3.5.0/1,M10.5.0", true},
		{"CST6CDT,M3.2.0,M11.1.0", true},
		{"AEST-10AEDT,M10.1.0,M4.1.0/3", true}, // DST across the year boundary
		{"UTC0", false},
		{"IST-5:30", false},
		{"MST7", false},
	}
	for _, tt := range tests {
		t.Run(tt.posix, func(t *testing.T) {
			loc, err := time.LoadLocationFromTZData(tt.posix, tzif(tt.posix, 0, "AAA"))
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			if got := observesDST(loc, 2026); got != tt.want {
				t.Errorf("observesDST(%q, 2026) = %v, want %v", tt.posix, got, tt.want)
			}
		})
	}
}

// TestObservesDSTSweepBounds pins both ends of the 1..366 sweep with zones
// whose entire DST window at noon is a single day — the first or the last of
// the year. An off-by-one at either bound would make observesDST report no DST
// for a perfectly good string, and the DST-dropped guard would then reject it.
//
// Both cases use 2028, a leap year, which is what makes day 366 December 31st
// rather than the next January 1st. That distinction is exactly what the upper
// bound has to get right, and it is also why the day-1 case needs a leap year:
// in a non-leap year sample 366 lands on the following January 1st, which by
// the annual periodicity of POSIX rules always mirrors day 1 anyway.
func TestObservesDSTSweepBounds(t *testing.T) {
	const leapYear = 2028

	tests := []struct {
		name    string
		posix   string
		wantDay int
	}{
		{
			// December 31st 2028 is a Sunday, so "last Sunday of December at
			// 01:00" fires that day, and "J1/0" ends DST at midnight on
			// January 1st, leaving the wrap-around part of the window empty.
			name:    "last day of the year",
			posix:   "XXX0YYY,M12.5.0/1,J1/0",
			wantDay: 366,
		},
		{
			// DST runs from 13:00 on December 31st (J365, which never counts
			// February 29th) to midnight on January 2nd. Noon on December 31st
			// is therefore still standard time, leaving January 1st as the
			// only day whose noon falls inside the window.
			name:    "first day of the year",
			posix:   "XXX0YYY,J365/13,J2/0",
			wantDay: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocationFromTZData(tt.posix, tzif(tt.posix, 0, "AAA"))
			if err != nil {
				t.Fatalf("setup: %v", err)
			}

			// Establish that the window really is confined to the one day, so
			// this cannot pass for the wrong reason if the rule ever parses
			// differently.
			var dstDays []int
			for yday := 1; yday <= 366; yday++ {
				if time.Date(leapYear, time.January, yday, 12, 0, 0, 0, loc).IsDST() {
					dstDays = append(dstDays, yday)
				}
			}
			if len(dstDays) != 1 || dstDays[0] != tt.wantDay {
				t.Fatalf("setup: DST at noon on days %v of %d, want exactly [%d]; "+
					"this case no longer isolates a sweep bound", dstDays, leapYear, tt.wantDay)
			}

			if !observesDST(loc, leapYear) {
				t.Errorf("observesDST missed a DST window confined to day %d; "+
					"the sweep is not reaching that end of the year", tt.wantDay)
			}
		})
	}
}

// TestParseYearSensitivity pins the reference-year injection: the DST-dropped
// guard probes the year of the supplied instant, not the wall clock.
func TestParseYearSensitivity(t *testing.T) {
	const posix = "GMT0BST,M3.5.0/1,M10.5.0"
	for _, year := range []int{2020, 2026, 2039} {
		ref := time.Date(year, time.June, 1, 0, 0, 0, 0, time.UTC)
		if _, err := parse(posix, ref); err != nil {
			t.Errorf("parse(%q) with reference year %d: %v", posix, year, err)
		}
	}
	// And the guard still fires regardless of the reference year.
	for _, year := range []int{2020, 2026, 2039} {
		ref := time.Date(year, time.June, 1, 0, 0, 0, 0, time.UTC)
		if _, err := parse("GMT0,M3.5.0/1,M10.5.0", ref); !errors.Is(err, ErrDSTDropped) {
			t.Errorf("parse with reference year %d error = %v, want ErrDSTDropped", year, err)
		}
	}
}

// TestDifferentialProbeSentinels is a white-box check that the two probe blobs
// really do disagree when tzset fails. If the sentinels were ever made equal —
// the obvious "cleanup" someone might apply to two nearly identical constants —
// guard 2 would silently stop rejecting anything, and every other test would
// still pass because the DST-dropped guard covers most malformed input.
func TestDifferentialProbeSentinels(t *testing.T) {
	if probeOffsetA == probeOffsetB {
		t.Error("probe sentinels share an offset; guard 2 cannot detect a parse failure")
	}
	if probeAbbrevA == probeAbbrevB {
		t.Error("probe sentinels share an abbreviation; guard 2 cannot detect a parse failure")
	}

	const garbage = "hello world"
	a, err := time.LoadLocationFromTZData("A", tzif(garbage, probeOffsetA, probeAbbrevA))
	if err != nil {
		t.Fatalf("setup A: %v", err)
	}
	b, err := time.LoadLocationFromTZData("B", tzif(garbage, probeOffsetB, probeAbbrevB))
	if err != nil {
		t.Fatalf("setup B: %v", err)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	nameA, offA := now.In(a).Zone()
	nameB, offB := now.In(b).Zone()
	if nameA != probeAbbrevA || offA != probeOffsetA {
		t.Errorf("blob A fell back to %s/%d, want the sentinel %s/%d",
			nameA, offA, probeAbbrevA, probeOffsetA)
	}
	if nameB != probeAbbrevB || offB != probeOffsetB {
		t.Errorf("blob B fell back to %s/%d, want the sentinel %s/%d",
			nameB, offB, probeAbbrevB, probeOffsetB)
	}
}

// TestParseIgnoresSentinelCollision guards the one input that looks like it
// could confuse the differential probe: a POSIX string that parses to exactly
// blob A's sentinel. Both blobs still agree, because tzset's result does not
// depend on the ttinfo record, so it is accepted — correctly, since it did
// parse.
func TestParseIgnoresSentinelCollision(t *testing.T) {
	loc, err := Parse(probeAbbrevA + "0")
	if err != nil {
		t.Fatalf("Parse(%q): %v", probeAbbrevA+"0", err)
	}
	if name, off := time.Now().In(loc).Zone(); name != probeAbbrevA || off != 0 {
		t.Errorf("got %s/%d, want %s/0", name, off, probeAbbrevA)
	}
}

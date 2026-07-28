// Package timezone turns the POSIX TZ string grow-app pushes over MQTT into a
// real *time.Location, validates it, and persists it.
//
// # The contract
//
// grow-app runs a device-agnostic timezone reconciler whose entire selector is
// component == "text" && objectId == "time_zone" && commandTopic != "". It
// publishes a bare POSIX TZ string — not IANA, not JSON — to the command topic,
// for example "GMT0BST,M3.5.0/1,M10.5.0", read verbatim out of the TZif v2+
// footer in its own /usr/share/zoneinfo. Values are at most 63 bytes. It
// decides push-versus-in-sync by comparing our retained state topic against the
// value it wants, so we must echo back the exact bytes we received.
//
// # Why a synthetic TZif blob
//
// Go's POSIX TZ parser — tzset, tzsetName, tzsetOffset, tzsetRule, tzsetNum at
// $GOROOT/src/time/zoneinfo.go:276-560 — is unexported, and there is no
// exported function that takes a TZ string. The obvious workaround is worse
// than useless: initLocal (zoneinfo_unix.go:28) treats $TZ as a zone *name or
// path*, so os.Setenv("TZ", "GMT0BST,M3.5.0/1,M10.5.0") does not error, it
// silently resolves to UTC. For a daemon whose whole job is knowing when local
// midnight falls, a silent one-hour-off is the worst available failure.
//
// tzset is nonetheless reachable through an exported API.
// time.LoadLocationFromTZData (zoneinfo_read.go:118) keeps the TZif footer
// verbatim and Location.lookup runs tzset on it:
//
//   - the footer is captured from a trailing "\n...\n" — zoneinfo_read.go:237-241
//   - timecnt == 0 is legal; a synthetic transition covering all time is
//     appended — zoneinfo_read.go:312-315
//   - so lookup lands on lo == len(tx)-1 and calls tzset on the extend
//     string — zoneinfo.go:209-213
//   - typecnt == 0 is rejected (zoneinfo_read.go:247-250), so the blob has to
//     carry exactly one otherwise-unused zone record
//
// So we hand LoadLocationFromTZData a minimal, valid TZif v2 file whose only
// real content is the POSIX footer, and the standard library parses the string
// for us. See tzif for the byte layout.
//
// The load-time cache is not a hazard here, which is worth stating because it
// looks like one: LoadLocationFromTZData pre-fills cacheStart/cacheEnd/cacheZone
// and lookup consults that cache first (zoneinfo.go:161-168). But the cache-fill
// path runs tzset itself and narrows the window to the transition tzset reports
// (zoneinfo_read.go:331-334), so instants outside the current DST window still
// fall through to the binary search and the extend string.
//
// # The rejected alternative
//
// Reverse-lookup — scanning /usr/share/zoneinfo for a zone whose footer matches
// — is deliberately not implemented. grow-app reads its footer from its
// container's tzdata while this Pi's tzdata updates independently, so the
// moment a government changes its DST rules the two footers diverge and the
// lookup either finds nothing or, worse, silently binds to a *different* zone
// that still carries the stale footer. It also answers the wrong question: the
// contract is "apply these rules", not "which zone currently has them" — those
// zones' full histories differ for past dates even when their footers agree.
//
// # Validation
//
// Three guards, mirroring esphome-components/timezone/timezone_text.cpp so this
// daemon and the fleet's ESP32 nodes are indistinguishable to the reconciler.
// See Parse.
package timezone

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxLen is the longest POSIX TZ string we accept, matching the ESPHome
// component's 64-byte buffer minus its NUL (timezone_text.cpp:102). grow-app
// documents the same 63-byte ceiling.
const MaxLen = 63

// Errors returned by Parse. They are distinguishable so callers can log the
// specific guard that fired; all of them mean "keep the zone you already have".
var (
	// ErrEmpty reports an empty string. Clearing an override is a separate
	// operation from parsing one — see Manager.Apply.
	ErrEmpty = errors.New("timezone: empty POSIX TZ string")

	// ErrTooLong reports a string longer than MaxLen.
	ErrTooLong = errors.New("timezone: POSIX TZ string too long")

	// ErrNUL reports an embedded NUL byte.
	ErrNUL = errors.New("timezone: POSIX TZ string contains a NUL byte")

	// ErrControlByte reports a control or non-ASCII byte.
	ErrControlByte = errors.New("timezone: POSIX TZ string contains a control byte")

	// ErrUnparsable reports that the standard library's tzset did not
	// understand the string.
	ErrUnparsable = errors.New("timezone: POSIX TZ string not understood")

	// ErrDSTDropped reports that the string named DST rules that were parsed
	// away to nothing.
	ErrDSTDropped = errors.New("timezone: POSIX TZ string DST rules were not understood")
)

// Sentinel zone records for the differential probe in Parse.
//
// The two blobs differ only in the single ttinfo record that tzset never
// consults, so any observable difference between the two Locations can only
// come from tzset having failed and lookup having fallen back to that record.
// The values are arbitrary; they only have to differ in both name and offset,
// since Time.Zone reports exactly those two.
const (
	probeOffsetA = 0
	probeOffsetB = 1234
	probeAbbrevA = "AAA"
	probeAbbrevB = "BBB"
)

// Parse converts a POSIX TZ string into a *time.Location.
//
// Three guards run, in order:
//
//  1. Byte guards (timezone_text.cpp:102-107): at most MaxLen bytes, no
//     embedded NUL. The ESPHome component rejects NULs because everything
//     downstream of it is C-string based and would truncate, while the
//     published state would carry the full byte string — state that was never
//     validated. We have no such truncation, but we reject them anyway so both
//     implementations accept exactly the same inputs.
//
//     This implementation widens that to every control and non-ASCII byte,
//     which is a deliberate departure from the ESPHome component and the only
//     one in this package. tzsetName accepts any run of three or more bytes
//     that are not digits, comma, plus or minus as a zone abbreviation
//     (zoneinfo.go:372-385), so "GMT0BST\n" parses — as a zone literally named
//     "BST\n", picking up tzcode's implicit US rules along the way
//     (zoneinfo.go:313-315). A newline is exactly what a hand-edit or a shell
//     redirect leaves in the state file, and a newline inside a zone name
//     silently corrupts every log line and MQTT payload that carries it. The
//     guard costs nothing in compatibility: the footers of all 499 files in a
//     current tzdata use only "+,-./0123456789:<>" and letters, so no real
//     value can be rejected by it. Everything printable stays tzset's business,
//     which is what keeps guard 2 free of assumptions about the grammar.
//
//  2. Parse guard. tzset failing is *silent*: lookup simply keeps the values
//     from the fallback ttinfo (zoneinfo.go:209-213 only overrides them when
//     ok is true). The differential probe below makes that observable.
//
//  3. DST-dropped guard (timezone_text.cpp:118-122). Go is lenient in the same
//     way ESPHome's parser is, in a different place — zoneinfo.go:295-298:
//
//     if len(s) == 0 || s[0] == ',' {
//     // No daylight savings time.
//     return stdName, stdOffset, lastTxSec, omega, false, true
//     }
//
//     So "GMT0,M3.5.0/1,M10.5.0" — the DST abbreviation missing — returns
//     ok=true as a fixed GMT0 with *both rules silently discarded*. A comma only
//     ever introduces DST rules, so a comma present alongside a zone that never
//     observes DST means part of the value was thrown away.
//
// Go is stricter than ESPHome on trailing garbage: zoneinfo.go:330 requires the
// rule pair to consume the whole string, so ESPHome's own documented typo
// "CST6CDT M3.2.0,M11.1.0/2" is rejected here outright. That is fine — being at
// least as strict as the fleet keeps us from accepting something a node would
// refuse.
//
// The returned Location's name is the POSIX string itself, so its String method
// reports what it was built from.
func Parse(posix string) (*time.Location, error) {
	return parse(posix, time.Now())
}

// parse is Parse with an injected reference time, so tests can pin the year the
// DST-dropped guard probes without depending on the wall clock.
func parse(posix string, now time.Time) (*time.Location, error) {
	// Guard 1: bytes.
	if posix == "" {
		return nil, ErrEmpty
	}
	if len(posix) > MaxLen {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrTooLong, len(posix), MaxLen)
	}
	for i := 0; i < len(posix); i++ {
		if b := posix[i]; b < 0x20 || b >= 0x7f {
			if b == 0 {
				return nil, ErrNUL
			}
			return nil, fmt.Errorf("%w: 0x%02x at offset %d", ErrControlByte, b, i)
		}
	}

	loc, err := time.LoadLocationFromTZData(posix, tzif(posix, probeOffsetA, probeAbbrevA))
	if err != nil {
		// Only reachable if tzif itself produced something malformed, since
		// the footer contents are never validated by the reader.
		return nil, fmt.Errorf("%w: %q: %w", ErrUnparsable, posix, err)
	}

	// Guard 2: the differential probe. Build the same footer against a
	// different fallback ttinfo and compare what the two Locations report at
	// one instant. If tzset parsed the string, both return tzset's values
	// verbatim and agree. If it failed, each falls through to its own sentinel
	// and they disagree. This assumes nothing whatsoever about the grammar,
	// which matters because the grammar is unexported and free to change.
	probe, err := time.LoadLocationFromTZData(posix, tzif(posix, probeOffsetB, probeAbbrevB))
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrUnparsable, posix, err)
	}
	nameA, offsetA := now.In(loc).Zone()
	nameB, offsetB := now.In(probe).Zone()
	if nameA != nameB || offsetA != offsetB {
		return nil, fmt.Errorf("%w: %q", ErrUnparsable, posix)
	}

	// Guard 3: DST rules named but discarded.
	if strings.Contains(posix, ",") && !observesDST(loc, now.Year()) {
		return nil, fmt.Errorf("%w: %q", ErrDSTDropped, posix)
	}

	return loc, nil
}

// observesDST reports whether loc is in daylight saving time at local noon on
// any day of the given year.
//
// Noon is sampled rather than midnight because a transition normally happens in
// the small hours, and a date arithmetic slip at the boundary is invisible in
// the middle of the day. Sampling every day of the year rather than reasoning
// about the rules keeps this independent of the unexported grammar and handles
// southern-hemisphere zones, whose DST window straddles the year boundary,
// without a special case.
//
// The accepted limit: a DST window shorter than 24 hours that happened to
// contain no local noon would be missed. No real tzdata footer produces one.
func observesDST(loc *time.Location, year int) bool {
	// Day-of-year 1..366 rather than iterating months: time.Date normalises
	// out-of-range days, so "January 366th" is December 31st in a leap year and
	// January 1st of the following year otherwise. That overshoot is harmless —
	// POSIX rules repeat every year, so next January 1st can only ever report
	// what this January 1st already did at yday 1 — and it is what lets one
	// loop bound serve both kinds of year.
	for yday := 1; yday <= 366; yday++ {
		if time.Date(year, time.January, yday, 12, 0, 0, 0, loc).IsDST() {
			return true
		}
	}
	return false
}

// tzif builds a minimal but strictly valid TZif version 2 file whose only
// meaningful content is the POSIX footer, plus one fallback zone record that
// exists solely because zoneinfo_read.go:247-250 rejects typecnt == 0.
//
// The layout, checked against the reader's own skip arithmetic
// (zoneinfo_read.go:178-188: skip = timecnt*4 + timecnt + typecnt*6 + charcnt +
// leapcnt*8 + isstdcnt + isutcnt, then += 4 + 16 for the v2 header it has just
// read — here 0+0+6+4+0+0+0+20 = 30, consumed from offset 44 to 74, followed by
// 24 bytes of repeated counts ending at 98):
//
//	offset  bytes  content
//	     0      4  "TZif"
//	     4     16  version '2' followed by 15 NUL
//	    20     24  six big-endian uint32 counts: isutcnt, isstdcnt, leapcnt,
//	               timecnt, typecnt, charcnt = 0, 0, 0, 0, 1, len(abbrev)+1
//	    44     10  v1 data: one ttinfo (int32 utoff, uint8 isdst, uint8
//	               desigidx) then the NUL-terminated abbreviation
//	    54     44  v2 header, byte-identical to offsets 0..44
//	    98     10  v2 data, byte-identical to offsets 44..54 (timecnt == 0, so
//	               there is no transition array in either block)
//	   108    n+2  "\n" + posix + "\n"
//
// desigidx 0 is valid because len(abbrev) > 0 (zoneinfo_read.go:268 rejects an
// index past the end of the abbreviation block) and byteString stops at the NUL
// (zoneinfo_read.go:105-110).
//
// The v1 block is required even though its contents are ignored: the reader
// skips it by computing its length from the counts it just read, so the bytes
// have to be there for the v2 header to land where the arithmetic expects.
func tzif(posix string, utoff int32, abbrev string) []byte {
	header := make([]byte, 0, 44)
	header = append(header, 'T', 'Z', 'i', 'f')
	header = append(header, '2')
	header = append(header, make([]byte, 15)...)
	for _, count := range [6]uint32{
		0,                       // isutcnt
		0,                       // isstdcnt
		0,                       // leapcnt
		0,                       // timecnt
		1,                       // typecnt
		uint32(len(abbrev) + 1), // charcnt: the abbreviation plus its NUL
	} {
		header = binary.BigEndian.AppendUint32(header, count)
	}

	body := make([]byte, 0, 6+len(abbrev)+1)
	body = binary.BigEndian.AppendUint32(body, uint32(utoff))
	body = append(body, 0) // isdst
	body = append(body, 0) // desigidx, into abbrev below
	body = append(body, abbrev...)
	body = append(body, 0)

	out := make([]byte, 0, 2*(len(header)+len(body))+len(posix)+2)
	out = append(out, header...) // v1 header
	out = append(out, body...)   // v1 data, skipped by the reader
	out = append(out, header...) // v2 header
	out = append(out, body...)   // v2 data
	out = append(out, '\n')
	out = append(out, posix...)
	out = append(out, '\n')
	return out
}

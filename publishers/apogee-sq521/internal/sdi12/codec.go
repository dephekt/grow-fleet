package sdi12

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Identity is the decoded reply to aI!.
//
// SDI-12 v1.3 fixes the field widths, so the layout is positional rather than
// delimited: address(1) version(2) vendor(8) model(6) firmware(3) serial(rest).
// The SQ-521 fills every field, e.g. "013APOGEE  SQ-5211043043".
type Identity struct {
	Address  byte
	Version  string // SDI-12 protocol version, "13" meaning v1.3
	Vendor   string
	Model    string
	Firmware string
	Serial   string
}

// identity field offsets, per SDI-12 v1.3 table 5.
const (
	idVersionStart  = 1
	idVendorStart   = 3
	idModelStart    = 11
	idFirmwareStart = 17
	idSerialStart   = 20 // serial number is variable width, up to 13 bytes
)

// Bounds on the two numeric fields of the "atttn" header.
//
// These are not stylistic limits, they are what keeps a length field read off a
// noisy wire from sizing an allocation. SDI-12 defines n as one digit for
// aM!/aV! and two for aC!, so 99 is the widest count the protocol can express;
// ttt is exactly three digits. Before the bound existed, a 14-byte line shaped
// <any byte><3 digits><10+ digits> — the ordinary "adapter spews noise" case —
// reached make() in [Client.collect] and killed the process: ten nines reserves
// 80 GB and dies with a *fatal* runtime out-of-memory that no recover() can
// catch, twenty overflows int to a negative capacity and panics in makeslice,
// and nine reserves 8 GB, which on a Pi is an OOM-kill. Fabricating a value (the
// Python's defect) is bad; taking the daemon down is worse.
const (
	maxValueCount   = 99
	maxReadySeconds = 999
)

// ParseMeasureHeader decodes the "atttn" reply to aM!/aV!: one address
// character, three digits of seconds-until-data-is-ready, then the number of
// values the sensor will return. "00013" means "ready in 1 s, 3 values".
//
// A line that does not fit that shape is ErrProtocol. This is the single most
// important fix in the port: the Python returned a fabricated (1, 1) for
// anything it could not match, which converted a framing desync into a
// plausible-looking guess. It would then wait a second, send D0! into the middle
// of somebody else's response, parse nothing, and return None on every
// subsequent poll — with no code path anywhere that resynchronised the port. The
// only recovery was a service restart. Surfacing ErrProtocol lets the caller
// drain and realign instead.
func ParseMeasureHeader(line string) (ready time.Duration, count int, err error) {
	s := strings.TrimSpace(line)
	if s == "" {
		// Silence is not garbage: the command is likely unsupported. The caller
		// retries next poll rather than resyncing.
		return 0, 0, fmt.Errorf("sdi12: empty measurement header: %w", ErrNoResponse)
	}
	// Equivalent to ^.(\d{3})(\d+)$ — hand-rolled to keep the hot path
	// allocation-free and the failure modes explicit.
	if len(s) < 5 {
		return 0, 0, fmt.Errorf("sdi12: measurement header %q too short: %w", line, ErrProtocol)
	}
	secs, ok := atoiDigits(s[1:4], maxReadySeconds) // ttt
	if !ok {
		return 0, 0, fmt.Errorf("sdi12: measurement header %q has no ttt field: %w", line, ErrProtocol)
	}
	// An out-of-range count is rejected here rather than clamped: a header
	// claiming more values than SDI-12 can express is not a count we misread,
	// it is a line we are not aligned to, and the caller must drain rather than
	// go on to collect from it. See maxValueCount for what this used to cost.
	n, ok := atoiDigits(s[4:], maxValueCount) // n, one or two digits
	if !ok {
		return 0, 0, fmt.Errorf("sdi12: measurement header %q has no value count in 0..%d: %w",
			line, maxValueCount, ErrProtocol)
	}
	return time.Duration(secs) * time.Second, n, nil
}

// ParseValues decodes a data response such as "0+156.9289" or, in darkness,
// "0-0.1147" (the SQ-521 legitimately reads slightly negative when dark, so
// negatives are data, not corruption).
//
// SDI-12 mandates an explicit sign in front of every value, which is what makes
// the fields self-delimiting: after dropping the address echo, each sign starts
// a new value. All of them are returned. The Python took findall(...)[0] and
// discarded the rest, which is precisely why we still do not know whether 0M!
// on this unit reports more than PPFD alone — the header's value count was
// parsed and then ignored.
//
// A bare address with no fields ("0") is a well-formed "I have no more data"
// reply and returns ErrNoValues, which is how the D0!/D1! continuation loop
// learns to stop.
func ParseValues(line string) ([]float64, error) {
	s := strings.TrimSpace(line)
	if s == "" {
		return nil, fmt.Errorf("sdi12: empty data response: %w", ErrNoResponse)
	}
	if !isSign(s[0]) {
		s = s[1:] // drop the address echo
	}
	if s == "" {
		return nil, fmt.Errorf("sdi12: data response %q carried no values: %w", line, ErrNoValues)
	}
	if !isSign(s[0]) {
		return nil, fmt.Errorf("sdi12: data response %q is not a signed field list: %w", line, ErrProtocol)
	}

	fields := splitSigned(s)
	values := make([]float64, 0, len(fields))
	for _, f := range fields {
		// Validate before ParseFloat: ParseFloat also accepts "+Inf", "+NaN" and
		// hex floats, none of which are SDI-12 and all of which would end up
		// serialised onto the MQTT bus for grow-app to choke on. A trailing CRC
		// (from a aMC!-style command we never issue) also lands here as garbage.
		if !isNumericField(f) {
			return nil, fmt.Errorf("sdi12: data response %q has malformed field %q: %w", line, f, ErrProtocol)
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, fmt.Errorf("sdi12: data response %q has unparseable field %q: %w", line, f, ErrProtocol)
		}
		values = append(values, v)
	}
	return values, nil
}

// ParseIdentity decodes the fixed-width aI! reply. Fields are bounds-checked
// explicitly and space-trimmed; anything shorter than the fixed portion is
// ErrProtocol rather than a silently truncated Identity.
//
// UNVERIFIED AGAINST HARDWARE — validate before release.
//
// The golden string "013APOGEE  SQ-5211043043" is internally consistent and the
// field widths come from SDI-12 v1.3 table 5, but where Firmware ends and Serial
// begins in *this* sensor's reply has never been checked against a real 0I!.
// "1043043" splitting as firmware "104" + serial "3043" is a guess. If Apogee
// pads differently, the daemon publishes a wrong sw_version and a wrong
// sensor_serial — both of which are sticky in Home Assistant's device registry
// and awkward to correct after the fact. Run 0I! against a live SQ-521, compare
// the serial to the sticker on the sensor body, and fix idFirmwareStart /
// idSerialStart before this ships.
func ParseIdentity(line string) (Identity, error) {
	s := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(s) == "" {
		return Identity{}, fmt.Errorf("sdi12: empty identity response: %w", ErrNoResponse)
	}
	if len(s) < idSerialStart {
		return Identity{}, fmt.Errorf("sdi12: identity response %q is %d bytes, need at least %d: %w",
			line, len(s), idSerialStart, ErrProtocol)
	}
	return Identity{
		Address:  s[0],
		Version:  strings.TrimSpace(s[idVersionStart:idVendorStart]),
		Vendor:   strings.TrimSpace(s[idVendorStart:idModelStart]),
		Model:    strings.TrimSpace(s[idModelStart:idFirmwareStart]),
		Firmware: strings.TrimSpace(s[idFirmwareStart:idSerialStart]),
		Serial:   strings.TrimSpace(s[idSerialStart:]),
	}, nil
}

// IsServiceRequest reports whether line is the sensor's bare address, which it
// transmits unprompted once a measurement has finished and the data is ready to
// be collected with D0!.
func IsServiceRequest(line string, addr byte) bool {
	return strings.TrimSpace(line) == string(addr)
}

func isSign(c byte) bool { return c == '+' || c == '-' }

// splitSigned cuts s at every sign, keeping the sign with the field that
// follows it. s must start with a sign.
func splitSigned(s string) []string {
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		if isSign(s[i]) {
			out = append(out, s[start:i])
			start = i
		}
	}
	return append(out, s[start:])
}

// isNumericField reports whether f is a sign, at least one digit, and at most
// one decimal point — the complete SDI-12 numeric grammar.
func isNumericField(f string) bool {
	if len(f) < 2 || !isSign(f[0]) {
		return false
	}
	digits, dots := 0, 0
	for i := 1; i < len(f); i++ {
		switch c := f[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			dots++
		default:
			return false
		}
	}
	return digits > 0 && dots <= 1
}

// atoiDigits parses s as an unsigned decimal in 0..max, rejecting anything that
// is not wall-to-wall digits (strconv.Atoi would happily accept a leading sign).
//
// The bound is checked as the value accumulates, so an arbitrarily long digit
// run costs nothing and can never overflow int on the way to being rejected —
// which matters because "9"×20 previously wrapped to a negative and reached
// make() as a capacity.
func atoiDigits(s string, max int) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > max {
			return 0, false
		}
	}
	return n, true
}

// Package config loads and validates the Apogee SQ-521 publisher's
// configuration from environment variables.
//
// The daemon runs as a systemd unit on quantum-sensor.home.arpa, so the
// environment is the only configuration surface: an EnvironmentFile plus a
// couple of systemd-provided variables (STATE_DIRECTORY). There is no config
// file to parse and no flags beyond --check-config.
//
// Two properties matter more than anything else here:
//
//  1. Validation aggregates. A misconfigured unit should report every problem
//     in one pass, because the feedback loop is "edit the env file, systemctl
//     restart, journalctl" — fixing one typo per restart is miserable.
//
//  2. Credentials never reach a log. String() redacts MQTTPassword, because
//     --check-config output ends up in terminals and pastes.
//
// The headline validation is the credential check. The Python publisher this
// replaces called username_pw_set() with an empty password and then
// connect_async()+loop_start(); paho's network thread retried the failing
// CONNECT forever in the background while publish() calls returned successfully
// into the client's queue. The logs looked healthy and nothing ever reached the
// bus. Refusing to start is the correct behaviour: a dead unit is visible,
// a lying one is not.
//
// MQTT_USERNAME and MQTT_PASSWORD are consequently both mandatory, and
// anonymous access is not supported. The broker on this bus requires
// credentials, so an anonymous configuration could only ever be a mistake — and
// every way of expressing one on purpose (an empty assignment, a lone space) is
// also a way to make it by accident. Coupling the password check to a non-empty
// username is what let MQTT_USERNAME=" " disable the whole check once already.
// There is deliberately no environment in which this daemon starts without
// credentials; if an anonymous broker ever becomes a real requirement, add an
// explicit MQTT_ANONYMOUS variable rather than inferring intent from an empty
// string.
package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Environment variable names. Exported-by-convention as constants so tests and
// String() name the same strings the operator writes in the unit file.
const (
	envNodeID          = "NODE_ID"
	envMQTTHost        = "MQTT_HOST"
	envMQTTPort        = "MQTT_PORT"
	envMQTTUsername    = "MQTT_USERNAME"
	envMQTTPassword    = "MQTT_PASSWORD"
	envTopicPrefix     = "MQTT_TOPIC_PREFIX"
	envSDI12Port       = "SDI12_PORT"
	envSDI12Address    = "SDI12_ADDRESS"
	envPollSeconds     = "POLL_SECONDS"
	envPPFDUnit        = "PPFD_UNIT"
	envReadTimeout     = "SDI12_READ_TIMEOUT"
	envWriteTimeout    = "SDI12_WRITE_TIMEOUT"
	envOfflineAfter    = "OFFLINE_AFTER"
	envShutdownTimeout = "SHUTDOWN_TIMEOUT"
	envStateDir        = "STATE_DIRECTORY"
)

// Defaults, held as strings so they flow through exactly the same parsing and
// validation path as operator-supplied values — a default that would fail
// validation is a bug we want the test suite to catch, not a special case.
const (
	defaultNodeID       = "quantum-sensor"
	defaultMQTTHost     = "192.168.8.3"
	defaultMQTTPort     = "1883"
	defaultMQTTUsername = "edge-daniel-home"
	defaultTopicPrefix  = "grow/daniel-home"
	// The by-id path rather than /dev/ttyUSB0 (which the Python used): the Pi
	// hosts other USB serial devices, and enumeration order is not stable
	// across reboots. by-id is keyed on the FTDI chip's serial number.
	defaultSDI12Port       = "/dev/serial/by-id/usb-FTDI_FT231X_USB_UART_D30GQUR5-if00-port0"
	defaultSDI12Address    = "0"
	defaultPollSeconds     = "10"
	defaultPPFDUnit        = "µmol/s/m²"
	defaultReadTimeout     = "2"
	defaultWriteTimeout    = "1"
	defaultOfflineAfter    = "3"
	defaultShutdownTimeout = "3"
	defaultStateDir        = ""
)

// maxTCPPort is the largest valid TCP port number.
const maxTCPPort = 65535

// Config is the fully-resolved, validated daemon configuration.
//
// A Config returned alongside a non-nil error is partially populated (fields
// whose values failed to parse hold their zero value) and must not be used to
// run the daemon. It is still safe to print via String(), which is what
// --check-config does when reporting failures.
type Config struct {
	// NodeID identifies this device on the bus. It becomes both an MQTT topic
	// segment and an InfluxDB tag in grow-history-recorder, so it must survive
	// as a literal in both.
	NodeID string

	// MQTTHost and MQTTPort address the broker.
	MQTTHost string
	MQTTPort int

	// MQTTUsername and MQTTPassword authenticate to the broker. Both are
	// required — see the package doc for why an empty one is fatal rather than
	// merely odd, and why anonymous access is not offered.
	MQTTUsername string
	MQTTPassword string

	// TopicPrefix is the site root ("grow/daniel-home"). Everything the daemon
	// publishes hangs off "<TopicPrefix>/<NodeID>", and discovery goes to
	// "<TopicPrefix>/_discovery". grow-app keys InfluxDB history on these
	// strings, so changing the prefix on a live install orphans history.
	TopicPrefix string

	// SDI12Port is the serial device for the Dr. Liu SDI-12->USB adapter.
	SDI12Port string

	// SDI12Address is the sensor's SDI-12 bus address, one character. Every
	// command is prefixed with it ("0M!", "0D0!").
	SDI12Address string

	// PollInterval is the gap between measurement cycles.
	PollInterval time.Duration

	// PPFDUnit is the unit_of_measurement published in the PPFD discovery
	// payload. It lands in an InfluxDB tag, so it is part of the wire contract
	// with grow-app and should not be changed casually.
	PPFDUnit string

	// ReadTimeout and WriteTimeout bound individual serial operations.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// OfflineAfter is how many consecutive failed measurement cycles are
	// tolerated before the daemon publishes a retained "offline" availability
	// message. One flaky read should not flap the device card in grow-app.
	OfflineAfter int

	// ShutdownTimeout bounds the graceful shutdown path (retained "offline",
	// broker disconnect, serial close) before the daemon exits anyway.
	ShutdownTimeout time.Duration

	// StateDir is systemd's StateDirectory. Empty means the unit did not grant
	// one, in which case persistence is disabled; the daemon logs that once at
	// startup rather than failing, since the sensor is perfectly useful without
	// it.
	StateDir string
}

// PersistenceEnabled reports whether a state directory was provided.
func (c Config) PersistenceEnabled() bool { return c.StateDir != "" }

// Load reads configuration from getenv, which production supplies as
// os.Getenv and tests supply as a map lookup. getenv must not be nil.
//
// All problems are collected and returned as a single joined error; Load never
// stops at the first one.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}

	var c Config

	c.NodeID = l.str(envNodeID, defaultNodeID)
	c.MQTTHost = l.str(envMQTTHost, defaultMQTTHost)
	c.MQTTUsername = l.str(envMQTTUsername, defaultMQTTUsername)
	// Deliberately untrimmed: leading/trailing whitespace is legal in a
	// password, and silently stripping it would produce an auth failure that
	// looks exactly like a wrong password.
	c.MQTTPassword = l.raw(envMQTTPassword, "")
	c.TopicPrefix = l.str(envTopicPrefix, defaultTopicPrefix)
	c.SDI12Port = l.str(envSDI12Port, defaultSDI12Port)
	c.SDI12Address = l.str(envSDI12Address, defaultSDI12Address)
	c.PPFDUnit = l.str(envPPFDUnit, defaultPPFDUnit)
	c.StateDir = l.str(envStateDir, defaultStateDir)

	// Numeric fields: each helper records at most one error per variable, so a
	// bad value never produces both a parse and a range complaint.
	if port, ok := l.integer(envMQTTPort, defaultMQTTPort); ok {
		c.MQTTPort = port
		if port < 1 || port > maxTCPPort {
			l.fail("%s %q: must be a TCP port in 1..%d", envMQTTPort, l.raw(envMQTTPort, defaultMQTTPort), maxTCPPort)
		}
	}
	if n, ok := l.integer(envOfflineAfter, defaultOfflineAfter); ok {
		c.OfflineAfter = n
		if n < 1 {
			l.fail("%s %q: must be at least 1 (number of consecutive failed cycles before publishing retained \"offline\")",
				envOfflineAfter, l.raw(envOfflineAfter, defaultOfflineAfter))
		}
	}

	// A poll faster than 1 s would hammer both the sensor (each cycle is three
	// SDI-12 measurement transactions, each with its own settling time) and the
	// broker with retained publishes.
	c.PollInterval = l.seconds(envPollSeconds, defaultPollSeconds, time.Second)
	// positive is the "any value greater than zero" floor; see loader.seconds.
	c.ReadTimeout = l.seconds(envReadTimeout, defaultReadTimeout, positive)
	c.WriteTimeout = l.seconds(envWriteTimeout, defaultWriteTimeout, positive)
	c.ShutdownTimeout = l.seconds(envShutdownTimeout, defaultShutdownTimeout, positive)

	// The headline check, in two independent halves. See the package doc.
	//
	// They must not be coupled ("username set but password empty"), because the
	// username can arrive empty in ways the operator never intended:
	// MQTT_USERNAME=" " survives raw's is-it-set test and then trims to "",
	// which under a coupled guard skips the password check entirely and starts
	// the daemon with no credentials at all — the exact failure mode this
	// package exists to prevent. Two independent checks also report two
	// problems, which is what an operator with a blank credential block needs
	// to see.
	if c.MQTTUsername == "" {
		l.fail("%s %q: must not be empty; the broker requires credentials and anonymous access is not supported",
			envMQTTUsername, l.raw(envMQTTUsername, defaultMQTTUsername))
	}
	// The password's own value is never interpolated into an error, here or
	// anywhere else; the message names the username instead so the operator
	// knows which credential pair is short.
	if c.MQTTPassword == "" {
		l.fail("%s: must not be empty (%s is %q); the client would never connect while publishes silently succeeded into its queue",
			envMQTTPassword, envMQTTUsername, c.MQTTUsername)
	}

	if c.MQTTHost == "" {
		l.fail("%s %q: must not be empty", envMQTTHost, l.raw(envMQTTHost, defaultMQTTHost))
	}

	switch {
	case c.NodeID == "":
		l.fail("%s %q: must not be empty", envNodeID, l.raw(envNodeID, defaultNodeID))
	case containsControl(c.NodeID):
		// Only flanking whitespace is trimmed, so an interior newline reaches
		// the wire. MQTT forbids U+0000 in a topic name outright and control
		// characters make the rest unusable as an InfluxDB tag; a newline also
		// breaks String()'s one-line-per-field shape, which is what
		// --check-config output is read as.
		l.fail("%s %q: must not contain control characters", envNodeID, c.NodeID)
	case strings.ContainsAny(c.NodeID, "/+#"):
		// NodeID is interpolated into topics and into an InfluxDB tag value:
		// "/" would split it across topic levels and "+"/"#" would make the
		// published topic collide with subscription wildcards.
		l.fail("%s %q: must not contain '/', '+' or '#' (it is both an MQTT topic segment and an InfluxDB tag)",
			envNodeID, c.NodeID)
	}

	switch {
	case c.TopicPrefix == "":
		l.fail("%s %q: must not be empty", envTopicPrefix, l.raw(envTopicPrefix, defaultTopicPrefix))
	case containsControl(c.TopicPrefix):
		// Same reasoning as NodeID: the prefix is the other half of every topic
		// this daemon publishes to.
		l.fail("%s %q: must not contain control characters", envTopicPrefix, c.TopicPrefix)
	case strings.HasPrefix(c.TopicPrefix, "/"), strings.HasSuffix(c.TopicPrefix, "/"):
		// Topics are built by concatenation ("<prefix>/<node>/..."), so a stray
		// slash produces an empty topic level, which is legal MQTT but not what
		// grow-app subscribes to.
		l.fail("%s %q: must not start or end with '/'", envTopicPrefix, c.TopicPrefix)
	case strings.ContainsAny(c.TopicPrefix, "+#"):
		l.fail("%s %q: must not contain the wildcard characters '+' or '#'", envTopicPrefix, c.TopicPrefix)
	}

	if !validAddress(c.SDI12Address) {
		// The raw value, like every other message here: an operator who typed a
		// space should see a space, not the "" that trimming leaves behind.
		l.fail("%s %q: must be exactly one character from [0-9a-zA-Z] (SDI-12 v1.4 sensor address)",
			envSDI12Address, l.raw(envSDI12Address, defaultSDI12Address))
	}

	if c.PPFDUnit == "" {
		l.fail("%s %q: must not be empty (it is published as unit_of_measurement and stored as an InfluxDB tag)",
			envPPFDUnit, l.raw(envPPFDUnit, defaultPPFDUnit))
	}

	// Only emptiness is checked. The device node is deliberately NOT stat'ed:
	// USB enumeration routinely lags a Pi reboot by seconds, and systemd may
	// start us first. A missing device is a runtime condition the serial layer
	// retries through, not a configuration error that should block startup.
	if c.SDI12Port == "" {
		l.fail("%s %q: must not be empty", envSDI12Port, l.raw(envSDI12Port, defaultSDI12Port))
	}

	return c, errors.Join(l.errs...)
}

// String renders every field one per line for --check-config.
//
// MQTTPassword is reported as <set>/<empty> and never as its value: this output
// is printed to a terminal and, historically, pasted into chat.
func (c Config) String() string {
	password := "<empty>"
	if c.MQTTPassword != "" {
		password = "<set>"
	}
	stateDir := c.StateDir
	if stateDir == "" {
		stateDir = "<empty> (persistence disabled)"
	}

	lines := []string{
		field("NodeID", envNodeID, c.NodeID),
		field("MQTTHost", envMQTTHost, c.MQTTHost),
		field("MQTTPort", envMQTTPort, c.MQTTPort),
		field("MQTTUsername", envMQTTUsername, c.MQTTUsername),
		field("MQTTPassword", envMQTTPassword, password),
		field("TopicPrefix", envTopicPrefix, c.TopicPrefix),
		field("SDI12Port", envSDI12Port, c.SDI12Port),
		field("SDI12Address", envSDI12Address, c.SDI12Address),
		field("PollInterval", envPollSeconds, c.PollInterval),
		field("PPFDUnit", envPPFDUnit, c.PPFDUnit),
		field("ReadTimeout", envReadTimeout, c.ReadTimeout),
		field("WriteTimeout", envWriteTimeout, c.WriteTimeout),
		field("OfflineAfter", envOfflineAfter, c.OfflineAfter),
		field("ShutdownTimeout", envShutdownTimeout, c.ShutdownTimeout),
		field("StateDir", envStateDir, stateDir),
	}
	return strings.Join(lines, "\n")
}

// field formats one "Name (ENV_VAR) value" line. The env var is included so an
// operator reading --check-config output knows which variable to edit.
//
// The column is 21 wide because the widest parenthesised variable name is
// "(SDI12_WRITE_TIMEOUT)", which is exactly 21 characters. At 20 that one row
// pushed its value a column right of every other, in output that is advertised
// as a byte-stable golden file.
func field(name, env string, value any) string {
	return fmt.Sprintf("%-16s %-21s %v", name, "("+env+")", value)
}

// positive is the floor passed to loader.seconds for durations that merely have
// to be greater than zero. Any value below 1ns is non-positive once rounded to
// a time.Duration.
const positive = time.Nanosecond

// loader threads the getenv func and the accumulating error list through
// parsing so every field can be attempted even after an earlier one failed.
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) fail(format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf(format, args...))
}

// raw returns the environment value, or def when the variable is unset.
//
// An unset variable and one set to the empty string are indistinguishable
// through getenv, so both fall back to the default. A variable set to
// whitespace does not: it survives here and is caught by the emptiness checks
// after str trims it, which is what turns MQTT_HOST=" " (an easy mistake in a
// quoted EnvironmentFile line) into a startup error instead of a mystery.
//
// That only holds while every str-backed field actually has an emptiness check.
// STATE_DIRECTORY is the sole field whose empty value is legitimate; any new
// one needs a check, or a stray space in the unit file becomes a silent "".
func (l *loader) raw(key, def string) string {
	if v := l.getenv(key); v != "" {
		return v
	}
	return def
}

// str returns the value with surrounding whitespace removed. Every string field
// except the password is an identifier, path or unit where flanking spaces are
// never meaningful and are almost always a stray character in the unit file.
func (l *loader) str(key, def string) string {
	return strings.TrimSpace(l.raw(key, def))
}

// integer parses a whole number, recording an error and reporting ok=false when
// the value is not one. Range checks belong at the call site so their messages
// can explain what the number means.
//
// strconv.Atoi's accepted syntax — an optional sign and decimal digits, nothing
// else — is the definition of "a number" for this package; seconds matches it.
func (l *loader) integer(key, def string) (int, bool) {
	raw := l.raw(key, def)
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		l.fail("%s %q: must be a whole number", key, raw)
		return 0, false
	}
	return n, true
}

// maxDurationSeconds is the largest whole number of seconds representable as a
// time.Duration, which is an int64 count of nanoseconds.
const maxDurationSeconds = math.MaxInt64 / int64(time.Second)

// seconds parses a duration expressed as a bare number of seconds ("10",
// "0.5") — the form the systemd unit and the Python original both use — and
// requires the result to be at least minimum.
//
// The accepted syntax is deliberately narrow, and is checked before the value
// reaches strconv.ParseFloat rather than delegated to it: ParseFloat takes the
// whole of Go's floating-point literal syntax, including underscore separators
// and hex-float mantissas, plus the words "NaN" and "Inf". That made
// POLL_SECONDS=1_0 mean 10s and POLL_SECONDS=0x1p4 mean 16s while
// MQTT_PORT=1_8_8_3 was rejected by Atoi — two parsers in one config file
// disagreeing about what a number is. decimalNumber is Atoi's syntax plus an
// optional fractional part.
//
// Go duration syntax ("10s") is rejected for a related reason: accepting both
// invites the reading where POLL_SECONDS=500 means 500ms.
//
// Passing minimum=positive expresses "greater than zero"; any larger floor is
// reported literally.
func (l *loader) seconds(key, def string, minimum time.Duration) time.Duration {
	raw := l.raw(key, def)
	s := strings.TrimSpace(raw)
	if !decimalNumber(s) {
		l.fail("%s %q: must be a number of seconds, e.g. %q", key, raw, def)
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// decimalNumber has already vetted the syntax, so the only failure left
		// is ErrRange: a literal too large for a float64, returned alongside
		// ±Inf. (Underflow is not an error — ParseFloat returns zero, which the
		// floor check below rejects on its own merits.)
		l.rangeFail(key, raw)
		return 0
	}

	// A time.Duration is an int64 of nanoseconds, and the Go spec leaves a
	// float-to-integer conversion *undefined* when the value does not fit. This
	// is not academic: amd64 yields INT64_MIN, which is negative and trips the
	// floor below by luck, while arm64 — the Pi this daemon is deployed to —
	// compiles the conversion to FCVTZS, which saturates to INT64_MAX. There,
	// POLL_SECONDS=1e11 would pass every check as a ~292-year interval and
	// produce a daemon that silently never polls. So the range is checked
	// before the conversion, not after.
	//
	// The upper bound is exclusive because float64(math.MaxInt64) rounds up to
	// exactly 2^63, one past the largest int64; every float64 strictly below it
	// converts exactly. NaN cannot reach here (decimalNumber rejects "NaN") and
	// would fail this comparison anyway, since every comparison against NaN is
	// false.
	nanos := math.Round(f * float64(time.Second))
	if !(nanos >= float64(math.MinInt64) && nanos < float64(math.MaxInt64)) {
		l.rangeFail(key, raw)
		return 0
	}

	d := time.Duration(nanos)
	if d < minimum {
		if minimum <= positive {
			l.fail("%s %q: must be greater than zero", key, raw)
		} else {
			l.fail("%s %q: must be at least %s", key, raw, minimum)
		}
	}
	return d
}

// rangeFail records a seconds value that cannot be represented as a
// time.Duration at all, which is a different complaint from failing a floor.
func (l *loader) rangeFail(key, raw string) {
	l.fail("%s %q: is out of range; a duration is an int64 of nanoseconds, so the magnitude must be at most %d seconds",
		key, raw, maxDurationSeconds)
}

// decimalNumber reports whether s is a plain decimal number: an optional sign,
// decimal digits and an optional fractional part, with at least one digit
// overall. Digits are ASCII by construction rather than by unicode.IsDigit,
// which would admit "١٠" that no strconv parser accepts.
func decimalNumber(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '+' || s[0] == '-' {
		s = s[1:]
	}
	digits, points := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			points++
		default:
			return false
		}
	}
	return digits > 0 && points <= 1
}

// containsControl reports whether s holds a Unicode control character. Such a
// character is never intentional in a topic segment: MQTT forbids U+0000 in a
// topic name and the rest survive no round trip an operator would recognise.
func containsControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// validAddress reports whether s is a legal SDI-12 sensor address: exactly one
// character from [0-9a-zA-Z]. Multi-byte runes fail the length check, which is
// correct — the address is written as a single ASCII byte on the wire.
func validAddress(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
